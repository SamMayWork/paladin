/*
 * Copyright © 2024 Kaleido, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on
 * an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package noto

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/hyperledger/firefly-signer/pkg/ethtypes"
	"github.com/kaleido-io/paladin/domains/noto/pkg/types"
	"github.com/kaleido-io/paladin/toolkit/pkg/algorithms"
	"github.com/kaleido-io/paladin/toolkit/pkg/domain"
	pb "github.com/kaleido-io/paladin/toolkit/pkg/prototk"
)

type transferHandler struct {
	noto *Noto
}

func (h *transferHandler) ValidateParams(ctx context.Context, params string) (interface{}, error) {
	var transferParams types.TransferParams
	if err := json.Unmarshal([]byte(params), &transferParams); err != nil {
		return nil, err
	}
	if transferParams.To == "" {
		return nil, fmt.Errorf("parameter 'to' is required")
	}
	if transferParams.Amount.BigInt().Sign() != 1 {
		return nil, fmt.Errorf("parameter 'amount' must be greater than 0")
	}
	return &transferParams, nil
}

func (h *transferHandler) Init(ctx context.Context, tx *types.ParsedTransaction, req *pb.InitTransactionRequest) (*pb.InitTransactionResponse, error) {
	params := tx.Params.(*types.TransferParams)

	return &pb.InitTransactionResponse{
		RequiredVerifiers: []*pb.ResolveVerifierRequest{
			{
				Lookup:    tx.DomainConfig.NotaryLookup,
				Algorithm: algorithms.ECDSA_SECP256K1_PLAINBYTES,
			},
			{
				Lookup:    tx.Transaction.From,
				Algorithm: algorithms.ECDSA_SECP256K1_PLAINBYTES,
			},
			{
				Lookup:    params.To,
				Algorithm: algorithms.ECDSA_SECP256K1_PLAINBYTES,
			},
		},
	}, nil
}

func (h *transferHandler) Assemble(ctx context.Context, tx *types.ParsedTransaction, req *pb.AssembleTransactionRequest) (*pb.AssembleTransactionResponse, error) {
	params := tx.Params.(*types.TransferParams)

	notary := domain.FindVerifier(tx.DomainConfig.NotaryLookup, algorithms.ECDSA_SECP256K1_PLAINBYTES, req.ResolvedVerifiers)
	if notary == nil || notary.Verifier != tx.DomainConfig.NotaryAddress {
		return nil, fmt.Errorf("notary resolved to unexpected address")
	}
	from := domain.FindVerifier(tx.Transaction.From, algorithms.ECDSA_SECP256K1_PLAINBYTES, req.ResolvedVerifiers)
	if from == nil {
		return nil, fmt.Errorf("error verifying recipient address")
	}
	fromAddress, err := ethtypes.NewAddress(from.Verifier)
	if err != nil {
		return nil, err
	}
	to := domain.FindVerifier(params.To, algorithms.ECDSA_SECP256K1_PLAINBYTES, req.ResolvedVerifiers)
	if to == nil {
		return nil, fmt.Errorf("error verifying recipient address")
	}
	toAddress, err := ethtypes.NewAddress(to.Verifier)
	if err != nil {
		return nil, err
	}

	inputCoins, inputStates, total, err := h.noto.prepareInputs(ctx, *fromAddress, params.Amount)
	if err != nil {
		return nil, err
	}
	outputCoins, outputStates, err := h.noto.prepareOutputs(*toAddress, params.Amount)
	if err != nil {
		return nil, err
	}
	if total.Cmp(params.Amount.BigInt()) == 1 {
		remainder := big.NewInt(0).Sub(total, params.Amount.BigInt())
		returnedCoins, returnedStates, err := h.noto.prepareOutputs(*fromAddress, ethtypes.NewHexInteger(remainder))
		if err != nil {
			return nil, err
		}
		outputCoins = append(outputCoins, returnedCoins...)
		outputStates = append(outputStates, returnedStates...)
	}

	var attestation []*pb.AttestationRequest
	switch tx.DomainConfig.Variant.String() {
	case types.NotoVariantDefault:
		encodedTransfer, err := h.noto.encodeTransferUnmasked(ctx, tx.ContractAddress, inputCoins, outputCoins)
		if err != nil {
			return nil, err
		}
		attestation = []*pb.AttestationRequest{
			// Sender confirms the initial request with a signature
			{
				Name:            "sender",
				AttestationType: pb.AttestationType_SIGN,
				Algorithm:       algorithms.ECDSA_SECP256K1_PLAINBYTES,
				Payload:         encodedTransfer,
				Parties:         []string{req.Transaction.From},
			},
			// Notary will endorse the assembled transaction (by submitting to the ledger)
			{
				Name:            "notary",
				AttestationType: pb.AttestationType_ENDORSE,
				Algorithm:       algorithms.ECDSA_SECP256K1_PLAINBYTES,
				Parties:         []string{tx.DomainConfig.NotaryLookup},
			},
		}
	case types.NotoVariantSelfSubmit:
		attestation = []*pb.AttestationRequest{
			// Notary will endorse the assembled transaction (by providing a signature)
			{
				Name:            "notary",
				AttestationType: pb.AttestationType_ENDORSE,
				Algorithm:       algorithms.ECDSA_SECP256K1_PLAINBYTES,
				Parties:         []string{tx.DomainConfig.NotaryLookup},
			},
			// Sender will endorse the assembled transaction (by submitting to the ledger)
			{
				Name:            "sender",
				AttestationType: pb.AttestationType_ENDORSE,
				Algorithm:       algorithms.ECDSA_SECP256K1_PLAINBYTES,
				Parties:         []string{req.Transaction.From},
			},
		}
	default:
		return nil, fmt.Errorf("unknown variant: %s", tx.DomainConfig.Variant)
	}

	return &pb.AssembleTransactionResponse{
		AssemblyResult: pb.AssembleTransactionResponse_OK,
		AssembledTransaction: &pb.AssembledTransaction{
			InputStates:  inputStates,
			OutputStates: outputStates,
		},
		AttestationPlan: attestation,
	}, nil
}

func (h *transferHandler) validateAmounts(coins *gatheredCoins) error {
	if coins.inTotal.Cmp(coins.outTotal) != 0 {
		return fmt.Errorf("invalid amount for 'transfer'")
	}
	return nil
}

func (h *transferHandler) validateSenderSignature(ctx context.Context, tx *types.ParsedTransaction, req *pb.EndorseTransactionRequest, coins *gatheredCoins) error {
	signature := domain.FindAttestation("sender", req.Signatures)
	if signature == nil {
		return fmt.Errorf("did not find 'sender' attestation")
	}
	if signature.Verifier.Lookup != tx.Transaction.From {
		return fmt.Errorf("sender attestation does not match transaction sender")
	}
	encodedTransfer, err := h.noto.encodeTransferUnmasked(ctx, tx.ContractAddress, coins.inCoins, coins.outCoins)
	if err != nil {
		return err
	}
	signingAddress, err := h.noto.recoverSignature(ctx, encodedTransfer, signature.Payload)
	if err != nil {
		return err
	}
	if signingAddress.String() != signature.Verifier.Verifier {
		return fmt.Errorf("sender signature does not match")
	}
	return nil
}

func (h *transferHandler) validateOwners(tx *types.ParsedTransaction, req *pb.EndorseTransactionRequest, coins *gatheredCoins) error {
	from := domain.FindVerifier(tx.Transaction.From, algorithms.ECDSA_SECP256K1_PLAINBYTES, req.ResolvedVerifiers)
	if from == nil {
		return fmt.Errorf("error verifying recipient address")
	}
	fromAddress, err := ethtypes.NewAddress(from.Verifier)
	if err != nil {
		return err
	}

	for i, coin := range coins.inCoins {
		if coin.Owner != *fromAddress {
			return fmt.Errorf("state %s is not owned by %s", coins.inStates[i].Id, tx.Transaction.From)
		}
	}
	return nil
}

func (h *transferHandler) Endorse(ctx context.Context, tx *types.ParsedTransaction, req *pb.EndorseTransactionRequest) (*pb.EndorseTransactionResponse, error) {
	coins, err := h.noto.gatherCoins(req.Inputs, req.Outputs)
	if err != nil {
		return nil, err
	}
	if err := h.validateAmounts(coins); err != nil {
		return nil, err
	}
	if err := h.validateOwners(tx, req, coins); err != nil {
		return nil, err
	}

	switch tx.DomainConfig.Variant.String() {
	case types.NotoVariantDefault:
		if req.EndorsementRequest.Name == "notary" {
			// Notary checks the signature from the sender, then submits the transaction
			if err := h.validateSenderSignature(ctx, tx, req, coins); err != nil {
				return nil, err
			}
			return &pb.EndorseTransactionResponse{
				EndorsementResult: pb.EndorseTransactionResponse_ENDORSER_SUBMIT,
			}, nil
		}
	case types.NotoVariantSelfSubmit:
		if req.EndorsementRequest.Name == "notary" {
			// Notary provides a signature for the assembled payload (to be verified on base ledger)
			inputIDs := make([]interface{}, len(req.Inputs))
			outputIDs := make([]interface{}, len(req.Outputs))
			for i, state := range req.Inputs {
				inputIDs[i] = state.Id
			}
			for i, state := range req.Outputs {
				outputIDs[i] = state.Id
			}
			data := ethtypes.HexBytes0xPrefix("")
			encodedTransfer, err := h.noto.encodeTransferMasked(ctx, tx.ContractAddress, inputIDs, outputIDs, data)
			if err != nil {
				return nil, err
			}
			return &pb.EndorseTransactionResponse{
				EndorsementResult: pb.EndorseTransactionResponse_SIGN,
				Payload:           encodedTransfer,
			}, nil
		} else if req.EndorsementRequest.Name == "sender" {
			if req.EndorsementVerifier.Lookup == tx.Transaction.From {
				// Sender submits the transaction
				return &pb.EndorseTransactionResponse{
					EndorsementResult: pb.EndorseTransactionResponse_ENDORSER_SUBMIT,
				}, nil
			}
		}
	default:
		return nil, fmt.Errorf("unknown variant: %s", tx.DomainConfig.Variant)
	}

	return nil, fmt.Errorf("unrecognized endorsement request: %s", req.EndorsementRequest.Name)
}

func (h *transferHandler) Prepare(ctx context.Context, tx *types.ParsedTransaction, req *pb.PrepareTransactionRequest) (*pb.PrepareTransactionResponse, error) {
	inputs := make([]string, len(req.InputStates))
	for i, state := range req.InputStates {
		inputs[i] = state.Id
	}
	outputs := make([]string, len(req.OutputStates))
	for i, state := range req.OutputStates {
		outputs[i] = state.Id
	}

	var signature *pb.AttestationResult
	switch tx.DomainConfig.Variant.String() {
	case types.NotoVariantDefault:
		// Include the signature from the sender
		// This is not verified on the base ledger, but can be verified by anyone with the unmasked state data
		signature = domain.FindAttestation("sender", req.AttestationResult)
		if signature == nil {
			return nil, fmt.Errorf("did not find 'sender' attestation")
		}
	case types.NotoVariantSelfSubmit:
		// Include the signature from the notary (will be verified on base ledger)
		signature = domain.FindAttestation("notary", req.AttestationResult)
		if signature == nil {
			return nil, fmt.Errorf("did not find 'notary' attestation")
		}
	default:
		return nil, fmt.Errorf("unknown variant: %s", tx.DomainConfig.Variant)
	}

	params := map[string]interface{}{
		"inputs":    inputs,
		"outputs":   outputs,
		"signature": ethtypes.HexBytes0xPrefix(signature.Payload),
		"data":      ethtypes.HexBytes0xPrefix(""),
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	functionJSON, err := json.Marshal(h.noto.contractABI.Functions()[tx.FunctionABI.Name])
	if err != nil {
		return nil, err
	}

	return &pb.PrepareTransactionResponse{
		Transaction: &pb.BaseLedgerTransaction{
			FunctionAbiJson: string(functionJSON),
			ParamsJson:      string(paramsJSON),
		},
	}, nil
}
