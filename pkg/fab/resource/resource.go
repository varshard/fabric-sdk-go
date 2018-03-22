/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

// Package resource provides access to fabric network resource management, typically using system channel queries.
package resource

import (
	reqContext "context"
	"net/http"

	"github.com/golang/protobuf/proto"
	"github.com/pkg/errors"

	"github.com/hyperledger/fabric-sdk-go/pkg/common/errors/multi"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/logging"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/providers/context"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/providers/fab"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/providers/msp"
	contextImpl "github.com/hyperledger/fabric-sdk-go/pkg/context"
	ccomm "github.com/hyperledger/fabric-sdk-go/pkg/core/config/comm"
	"github.com/hyperledger/fabric-sdk-go/pkg/fab/resource/api"
	"github.com/hyperledger/fabric-sdk-go/pkg/fab/txn"
	"github.com/hyperledger/fabric-sdk-go/third_party/github.com/hyperledger/fabric/protos/common"
	pb "github.com/hyperledger/fabric-sdk-go/third_party/github.com/hyperledger/fabric/protos/peer"
)

var logger = logging.NewLogger("fabsdk/fab")

type fabCtx struct {
	context.Providers
	msp.SigningIdentity
}

// SignChannelConfig signs a configuration.
func SignChannelConfig(ctx context.Client, config []byte, signer msp.SigningIdentity) (*common.ConfigSignature, error) {
	logger.Debug("SignChannelConfig - start")

	if config == nil {
		return nil, errors.New("channel configuration required")
	}

	signingUser := signer
	// If signing user is not provided default to client's user context
	if signingUser == nil {
		signingUser = ctx
	}

	if signingUser == nil {
		return nil, errors.New("user context required")
	}

	sigCtx := fabCtx{
		Providers:       ctx,
		SigningIdentity: signingUser,
	}

	return CreateConfigSignature(sigCtx, config)
}

// CreateChannel calls the orderer to start building the new channel.
func CreateChannel(reqCtx reqContext.Context, request api.CreateChannelRequest) (fab.TransactionID, error) {
	if request.Orderer == nil {
		return fab.EmptyTransactionID, errors.New("missing orderer request parameter for the initialize channel")
	}

	if request.Name == "" {
		return fab.EmptyTransactionID, errors.New("missing name request parameter for the new channel")
	}

	if request.Envelope != nil {
		return createChannelFromEnvelope(reqCtx, request)
	}

	if request.Config == nil {
		return fab.EmptyTransactionID, errors.New("missing envelope request parameter containing the configuration of the new channel")
	}

	if request.Signatures == nil {
		return fab.EmptyTransactionID, errors.New("missing signatures request parameter for the new channel")
	}

	ctx, ok := contextImpl.RequestClientContext(reqCtx)
	if !ok {
		return fab.EmptyTransactionID, errors.New("creation of transaction header failed, failed to extract client context from reqContext")
	}
	txh, err := txn.NewHeader(ctx, request.Name)
	if err != nil {
		return fab.EmptyTransactionID, errors.WithMessage(err, "creation of transaction header failed")
	}

	return txh.TransactionID(), createOrUpdateChannel(reqCtx, txh, request)
}

// TODO: this function was extracted from createOrUpdateChannel, but needs a closer examination.
func createChannelFromEnvelope(reqCtx reqContext.Context, request api.CreateChannelRequest) (fab.TransactionID, error) {
	env, err := extractSignedEnvelope(request.Envelope)
	if err != nil {
		return fab.EmptyTransactionID, errors.WithMessage(err, "signed envelope not valid")
	}
	// Send request
	_, err = request.Orderer.SendBroadcast(reqCtx, env)
	if err != nil {
		return fab.EmptyTransactionID, errors.WithMessage(err, "failed broadcast to orderer")
	}
	return fab.EmptyTransactionID, nil
}

// GenesisBlockFromOrderer returns the genesis block from the defined orderer that may be
// used in a join request
func GenesisBlockFromOrderer(reqCtx reqContext.Context, channelName string, orderer fab.Orderer) (*common.Block, error) {
	return retrieveBlock(reqCtx, []fab.Orderer{orderer}, channelName, newSpecificSeekPosition(0))
}

// LastConfigFromOrderer fetches the current configuration block for the specified channel
// from the given orderer
func LastConfigFromOrderer(reqCtx reqContext.Context, channelName string, orderer fab.Orderer) (*common.Block, error) {
	logger.Debugf("channelConfig - start for channel %s", channelName)

	// Get the newest block
	block, err := retrieveBlock(reqCtx, []fab.Orderer{orderer}, channelName, newNewestSeekPosition())
	if err != nil {
		return nil, err
	}
	logger.Debugf("channelConfig - Retrieved newest block number: %d\n", block.Header.Number)

	// Get the index of the last config block
	lastConfig, err := GetLastConfigFromBlock(block)
	if err != nil {
		return nil, errors.Wrap(err, "GetLastConfigFromBlock failed")
	}
	logger.Debugf("channelConfig - Last config index: %d\n", lastConfig.Index)

	// Get the last config block
	block, err = retrieveBlock(reqCtx, []fab.Orderer{orderer}, channelName, newSpecificSeekPosition(lastConfig.Index))
	if err != nil {
		return nil, errors.WithMessage(err, "retrieve block failed")
	}
	logger.Debugf("channelConfig - Last config block number %d, Number of tx: %d", block.Header.Number, len(block.Data.Data))

	if len(block.Data.Data) != 1 {
		return nil, errors.New("apiconfig block must contain one transaction")
	}

	return block, nil
}

// JoinChannel sends a join channel proposal to the target peer.
//
// TODO extract targets from request into parameter.
func JoinChannel(reqCtx reqContext.Context, request api.JoinChannelRequest, targets []fab.ProposalProcessor) error {

	if request.GenesisBlock == nil {
		return errors.New("missing block input parameter with the required genesis block")
	}

	cir, err := createJoinChannelInvokeRequest(request.GenesisBlock)
	if err != nil {
		return errors.WithMessage(err, "creation of join channel invoke request failed")
	}

	_, err = queryChaincode(reqCtx, cir, targets)
	return err
}

func extractSignedEnvelope(reqEnvelope []byte) (*fab.SignedEnvelope, error) {
	envelope := &common.Envelope{}
	err := proto.Unmarshal(reqEnvelope, envelope)
	if err != nil {
		return nil, errors.Wrap(err, "unmarshal request envelope failed")
	}
	se := fab.SignedEnvelope{
		Signature: envelope.Signature,
		Payload:   envelope.Payload,
	}
	return &se, nil
}

// createOrUpdateChannel creates a new channel or updates an existing channel.
func createOrUpdateChannel(reqCtx reqContext.Context, txh *txn.TransactionHeader, request api.CreateChannelRequest) error {

	configUpdateEnvelope := &common.ConfigUpdateEnvelope{
		ConfigUpdate: request.Config,
		Signatures:   request.Signatures,
	}
	configUpdateEnvelopeBytes, err := proto.Marshal(configUpdateEnvelope)
	if err != nil {
		return errors.Wrap(err, "marshal configUpdateEnvelope failed")
	}

	ctx, ok := contextImpl.RequestClientContext(reqCtx)
	if !ok {
		return errors.New("failed get client context from reqContext for Creating ChannelHeader")
	}
	channelHeaderOpts := txn.ChannelHeaderOpts{
		TxnHeader:   txh,
		TLSCertHash: ccomm.TLSCertHash(ctx.Config()),
	}
	channelHeader, err := txn.CreateChannelHeader(common.HeaderType_CONFIG_UPDATE, channelHeaderOpts)
	if err != nil {
		return errors.WithMessage(err, "CreateChannelHeader failed")
	}

	payload, err := txn.CreatePayload(txh, channelHeader, configUpdateEnvelopeBytes)
	if err != nil {
		return errors.WithMessage(err, "CreatePayload failed")
	}

	_, err = txn.BroadcastPayload(reqCtx, payload, []fab.Orderer{request.Orderer})
	if err != nil {
		return errors.WithMessage(err, "SendEnvelope failed")
	}
	return nil
}

// QueryChannels queries the names of all the channels that a peer has joined.
func QueryChannels(reqCtx reqContext.Context, peer fab.ProposalProcessor) (*pb.ChannelQueryResponse, error) {

	if peer == nil {
		return nil, errors.New("peer required")
	}

	cir := createChannelsInvokeRequest()
	payload, err := queryChaincodeWithTarget(reqCtx, cir, peer)
	if err != nil {
		return nil, errors.WithMessage(err, "cscc.GetChannels failed")
	}

	response := new(pb.ChannelQueryResponse)
	err = proto.Unmarshal(payload, response)
	if err != nil {
		return nil, errors.Wrap(err, "unmarshal ChannelQueryResponse failed")
	}
	return response, nil
}

// QueryInstalledChaincodes queries the installed chaincodes on a peer.
// Returns the details of all chaincodes installed on a peer.
func QueryInstalledChaincodes(reqCtx reqContext.Context, peer fab.ProposalProcessor) (*pb.ChaincodeQueryResponse, error) {

	if peer == nil {
		return nil, errors.New("peer required")
	}

	cir := createInstalledChaincodesInvokeRequest()
	payload, err := queryChaincodeWithTarget(reqCtx, cir, peer)
	if err != nil {
		return nil, errors.WithMessage(err, "lscc.getinstalledchaincodes failed")
	}

	response := new(pb.ChaincodeQueryResponse)
	err = proto.Unmarshal(payload, response)
	if err != nil {
		return nil, errors.Wrap(err, "unmarshal ChaincodeQueryResponse failed")
	}

	return response, nil
}

// InstallChaincode sends an install proposal to one or more endorsing peers.
func InstallChaincode(reqCtx reqContext.Context, req api.InstallChaincodeRequest, targets []fab.ProposalProcessor) ([]*fab.TransactionProposalResponse, fab.TransactionID, error) {

	if req.Name == "" {
		return nil, fab.EmptyTransactionID, errors.New("chaincode name required")
	}
	if req.Path == "" {
		return nil, fab.EmptyTransactionID, errors.New("chaincode path required")
	}
	if req.Version == "" {
		return nil, fab.EmptyTransactionID, errors.New("chaincode version required")
	}
	if req.Package == nil {
		return nil, fab.EmptyTransactionID, errors.New("chaincode package is required")
	}

	propReq := ChaincodeInstallRequest{
		Name:    req.Name,
		Path:    req.Path,
		Version: req.Version,
		Package: &ChaincodePackage{
			Type: req.Package.Type,
			Code: req.Package.Code,
		},
	}

	ctx, ok := contextImpl.RequestClientContext(reqCtx)
	if !ok {
		return nil, fab.EmptyTransactionID, errors.New("failed get client context from reqContext for txn header")
	}

	txh, err := txn.NewHeader(ctx, fab.SystemChannel)
	if err != nil {
		return nil, fab.EmptyTransactionID, errors.WithMessage(err, "create transaction ID failed")
	}

	prop, err := CreateChaincodeInstallProposal(txh, propReq)
	if err != nil {
		return nil, fab.EmptyTransactionID, errors.WithMessage(err, "creation of install chaincode proposal failed")
	}

	transactionProposalResponse, err := txn.SendProposal(reqCtx, prop, targets)

	return transactionProposalResponse, prop.TxnID, err
}

func queryChaincode(reqCtx reqContext.Context, request fab.ChaincodeInvokeRequest, targets []fab.ProposalProcessor) ([][]byte, error) {
	var errors multi.Errors
	responses := [][]byte{}

	for _, target := range targets {
		resp, err := queryChaincodeWithTarget(reqCtx, request, target)
		responses = append(responses, resp)
		if err != nil {
			errors = append(errors, err)
		}
	}

	return responses, errors.ToError()
}

func queryChaincodeWithTarget(reqCtx reqContext.Context, request fab.ChaincodeInvokeRequest, target fab.ProposalProcessor) ([]byte, error) {

	targets := []fab.ProposalProcessor{target}

	ctx, ok := contextImpl.RequestClientContext(reqCtx)
	if !ok {
		return nil, errors.New("failed get client context from reqContext for txn header")
	}

	txh, err := txn.NewHeader(ctx, fab.SystemChannel)
	if err != nil {
		return nil, errors.WithMessage(err, "create transaction ID failed")
	}

	tp, err := txn.CreateChaincodeInvokeProposal(txh, request)
	if err != nil {
		return nil, errors.WithMessage(err, "NewProposal failed")
	}

	tpr, err := txn.SendProposal(reqCtx, tp, targets)
	if err != nil {
		return nil, errors.WithMessage(err, "SendProposal failed")
	}

	err = validateResponse(tpr[0])
	if err != nil {
		return nil, errors.WithMessage(err, "transaction proposal failed")
	}

	return tpr[0].ProposalResponse.GetResponse().Payload, nil
}

func validateResponse(response *fab.TransactionProposalResponse) error {
	if response.Status != http.StatusOK {
		return errors.Errorf("bad status from %s (%d)", response.Endorser, response.Status)
	}

	return nil
}
