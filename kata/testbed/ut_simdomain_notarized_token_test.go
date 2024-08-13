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

package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"testing"

	_ "embed"

	"github.com/hyperledger/firefly-signer/pkg/ethtypes"
	"github.com/kaleido-io/paladin/kata/internal/confutil"
	"github.com/kaleido-io/paladin/kata/internal/filters"
	"github.com/kaleido-io/paladin/kata/pkg/proto"
	"github.com/kaleido-io/paladin/kata/pkg/signer"
	"github.com/kaleido-io/paladin/kata/pkg/types"
	"github.com/stretchr/testify/assert"
	pb "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

//go:embed abis/SIMDomain.json
var simDomainBuild []byte // comes from Hardhat build

//go:embed abis/SIMToken.json
var simTokenBuild []byte // comes from Hardhat build

func toJSONString(t *testing.T, v interface{}) string {
	b, err := json.Marshal(v)
	assert.NoError(t, err)
	return string(b)
}

// Example of how someone might use this testbed externally
func TestDemoNotarizedCoinSelection(t *testing.T) {

	simDomainABI := mustParseBuildABI(simDomainBuild)
	simTokenABI := mustParseBuildABI(simTokenBuild)

	fakeCoinConstructorABI := `{
		"type": "constructor",
		"inputs": [
		  {
		    "name": "notary",
			"type": "string"
		  },
		  {
		    "name": "name",
			"type": "string"
		  },
		  {
		    "name": "symbol",
			"type": "string"
		  }
		],
		"outputs": null
	}`

	fakeCoinStateSchema := `{
		"type": "tuple",
		"internalType": "struct FakeCoin",
		"components": [
			{
				"name": "salt",
				"type": "bytes32"
			},
			{
				"name": "owner",
				"type": "address",
				"indexed": true
			},
			{
				"name": "amount",
				"type": "uint256",
				"indexed": true
			}
		]
	}`

	// Note, here we're simulating a domain that choose to support versions of a "Transfer" function
	// with "string" types (rather than "address") for the from/to address and to ask Paladin to do
	// verifier resolution for these. The same domain could also support "address" type inputs/outputs
	// in the same ABI.
	fakeCoinTransferABI := `{
		"type": "function",
		"name": "transfer",
		"inputs": [
		  {
		    "name": "from",
			"type": "string"
		  },
		  {
		    "name": "to",
			"type": "string"
		  },
		  {
		    "name": "amount",
			"type": "uint256"
		  }
		],
		"outputs": null
	}`

	fakeDeployPayload := `{
		"notary": "domain1/contract1/notary",
		"name": "FakeToken1",
		"symbol": "FT1"
	}`

	type fakeTransferParser struct {
		From   string               `json:"from,omitempty"`
		To     string               `json:"to,omitempty"`
		Amount *ethtypes.HexInteger `json:"amount"`
	}

	type fakeCoinParser struct {
		Salt   ethtypes.HexBytes0xPrefix `json:"salt"`
		Owner  ethtypes.Address0xHex     `json:"owner"`
		Amount *ethtypes.HexInteger      `json:"amount"`
	}

	fakeTransferMintPayload := `{
		"from": "",
		"to": "wallets/org1/aaaaaa",
		"amount": "123000000000000000000"
	}`

	fakeTransferPayload1 := `{
		"from": "wallets/org1/aaaaaa",
		"to": "wallets/org2/bbbbbb",
		"amount": "23000000000000000000"
	}`

	var factoryAddr ethtypes.Address0xHex
	var fakeCoinSchemaID string
	var domainUUID string

	fakeCoinSelection := func(sc simCallbacks, fromAddr *ethtypes.Address0xHex, amount *big.Int) ([]string, *big.Int, error) {
		var lastStateTimestamp int64
		total := big.NewInt(0)
		stateIDs := []string{}
		for {
			// Simple oldest coin first algo
			query := &filters.QueryJSON{
				Limit: confutil.P(10),
				Sort:  []string{".created"},
				FilterJSON: filters.FilterJSON{
					FilterJSONOps: filters.FilterJSONOps{
						Eq: []*filters.FilterJSONKeyValue{
							{FilterJSONBase: filters.FilterJSONBase{Field: "owner"}, Value: types.RawJSON(fmt.Sprintf(`"%s"`, fromAddr))},
						},
					},
				},
			}
			if lastStateTimestamp > 0 {
				query.GT = []*filters.FilterJSONKeyValue{
					{FilterJSONBase: filters.FilterJSONBase{Field: ".created"}, Value: types.RawJSON(strconv.FormatInt(lastStateTimestamp, 10))},
				}
			}
			states, err := sc.FindAvailableStates(domainUUID, fakeCoinSchemaID, query)
			if err != nil {
				return nil, nil, err
			}
			if len(states) == 0 {
				return nil, nil, fmt.Errorf("insufficient funds (available=%s)", total.Text(10))
			}
			for _, state := range states {
				lastStateTimestamp = state.StoredAt
				// Note: More sophisticated coin selection might prefer states that aren't locked to a sequence
				var coin fakeCoinParser
				if err := json.Unmarshal([]byte(state.DataJson), &coin); err != nil {
					return nil, nil, fmt.Errorf("coin %s is invalid: %s", state.HashId, err)
				}
				total = total.Add(total, coin.Amount.BigInt())
				stateIDs = append(stateIDs, state.HashId)
				if total.Cmp(amount) >= 0 {
					// We've got what we need - return how much over we are
					return stateIDs, new(big.Int).Sub(amount, total), nil
				}
			}
		}
	}

	_, rpcCall, done := newDomainSimulator(t, map[protoreflect.FullName]domainSimulatorFn{

		CONFIGURE_DOMAIN: func(_ simCallbacks, iReq pb.Message) (pb.Message, error) {
			req := iReq.(*proto.ConfigureDomainRequest)
			assert.Equal(t, "domain1", req.Name)
			assert.JSONEq(t, `{"some":"config"}`, req.ConfigYaml)
			assert.Equal(t, int64(1337), req.ChainId) // from tools/besu_bootstrap
			return &proto.ConfigureDomainResponse{
				DomainConfig: &proto.DomainConfig{
					ConstructorAbiJson:     fakeCoinConstructorABI,
					FactoryContractAddress: factoryAddr.String(), // note this requires testbed_deployBytecode to have completed
					FactoryContractAbiJson: toJSONString(t, simDomainABI),
					PrivateContractAbiJson: toJSONString(t, simTokenABI),
					AbiStateSchemasJson:    []string{fakeCoinStateSchema},
				},
			}, nil
		},

		INIT_DOMAIN: func(_ simCallbacks, iReq pb.Message) (pb.Message, error) {
			req := iReq.(*proto.InitDomainRequest)
			assert.Len(t, req.AbiStateSchemas, 1)
			fakeCoinSchemaID = req.AbiStateSchemas[0].Id
			assert.Equal(t, "type=FakeCoin(bytes32 salt,address owner,uint256 amount),labels=[owner,amount]", req.AbiStateSchemas[0].Signature)
			domainUUID = req.DomainUuid
			return &proto.InitDomainResponse{}, nil
		},

		INIT_DEPLOY: func(_ simCallbacks, iReq pb.Message) (pb.Message, error) {
			req := iReq.(*proto.InitDeployTransactionRequest)
			assert.JSONEq(t, fakeCoinConstructorABI, req.Transaction.ConstructorAbi)
			assert.JSONEq(t, fakeDeployPayload, req.Transaction.ConstructorParamsJson)
			return &proto.InitDeployTransactionResponse{
				RequiredVerifiers: []*proto.ResolveVerifierRequest{
					{
						Lookup:    "domain1/contract1/notary",
						Algorithm: signer.Algorithm_ECDSA_SECP256K1_PLAINBYTES,
					},
				},
			}, nil
		},

		PREPARE_DEPLOY: func(_ simCallbacks, iReq pb.Message) (pb.Message, error) {
			req := iReq.(*proto.PrepareDeployTransactionRequest)
			assert.JSONEq(t, fakeCoinConstructorABI, req.Transaction.ConstructorAbi)
			assert.JSONEq(t, `{
				"notary": "domain1/contract1/notary",
				"name": "FakeToken1",
				"symbol": "FT1"
			}`, req.Transaction.ConstructorParamsJson)
			assert.Len(t, req.ResolvedVerifiers, 1)
			assert.Equal(t, signer.Algorithm_ECDSA_SECP256K1_PLAINBYTES, req.ResolvedVerifiers[0].Algorithm)
			assert.Equal(t, "domain1/contract1/notary", req.ResolvedVerifiers[0].Lookup)
			assert.NotEmpty(t, req.ResolvedVerifiers[0].Verifier)
			return &proto.PrepareDeployTransactionResponse{
				Transaction: &proto.BaseLedgerTransaction{
					FunctionName: "newSIMTokenNotarized",
					ParamsJson: fmt.Sprintf(`{
						"txId": "%s",
						"notary": "%s"
					}`, req.Transaction.TransactionId, req.ResolvedVerifiers[0].Verifier),
					SigningAddress: fmt.Sprintf("domain1/contract1/onetimekeys/%s", req.Transaction.TransactionId),
				},
			}, nil
		},

		INIT_TRANSACTION: func(_ simCallbacks, iReq pb.Message) (pb.Message, error) {
			req := iReq.(*proto.InitTransactionRequest)
			assert.JSONEq(t, fakeCoinTransferABI, req.Transaction.FunctionAbiJson)
			assert.Equal(t, "transfer(string,string,uint256)", req.Transaction.FunctionSignature)
			var inputs fakeTransferParser
			err := json.Unmarshal([]byte(req.Transaction.FunctionParamsJson), &inputs)
			assert.NoError(t, err)
			assert.Greater(t, inputs.Amount.BigInt().Sign(), 0)
			// We require ethereum addresses for the "from" and "to" addresses to actually
			// execute the transaction. See notes above about this.
			requiredVerifiers := []*proto.ResolveVerifierRequest{
				{
					Lookup:    "domain1/contract1/notary",
					Algorithm: signer.Algorithm_ECDSA_SECP256K1_PLAINBYTES,
				},
			}
			if inputs.From != "" {
				requiredVerifiers = append(requiredVerifiers, &proto.ResolveVerifierRequest{
					Lookup:    inputs.From,
					Algorithm: signer.Algorithm_ECDSA_SECP256K1_PLAINBYTES,
				})
			}
			if inputs.To != "" && (inputs.From == "" || inputs.From != inputs.To) {
				requiredVerifiers = append(requiredVerifiers, &proto.ResolveVerifierRequest{
					Lookup:    inputs.To,
					Algorithm: signer.Algorithm_ECDSA_SECP256K1_PLAINBYTES,
				})
			}
			return &proto.InitTransactionResponse{
				RequiredVerifiers: requiredVerifiers,
			}, nil
		},

		ASSEMBLE_TRANSACTION: func(sc simCallbacks, iReq pb.Message) (pb.Message, error) {
			req := iReq.(*proto.AssembleTransactionRequest)
			var inputs fakeTransferParser
			err := json.Unmarshal([]byte(req.Transaction.FunctionParamsJson), &inputs)
			assert.NoError(t, err)
			amount := inputs.Amount.BigInt()
			toKeep := new(big.Int)
			var fromAddr *ethtypes.Address0xHex
			var toAddr *ethtypes.Address0xHex
			for _, v := range req.ResolvedVerifiers {
				if inputs.From != "" && v.Lookup == inputs.From {
					fromAddr = ethtypes.MustNewAddress(v.Verifier)
				}
				if inputs.To != "" && v.Lookup == inputs.To {
					toAddr = ethtypes.MustNewAddress(v.Verifier)
				}
			}
			coinsToSpend := []string{}
			if inputs.From != "" {
				coinsToSpend, toKeep, err = fakeCoinSelection(sc, fromAddr, amount)
				if err != nil {
					return nil, err
				}
			}
			newStates := []*proto.StateData{}
			if fromAddr != nil && toKeep.Sign() > 0 {
				// Generate a state to keep for ourselves
				newStates = append(newStates, &proto.StateData{
					SchemaId: fakeCoinSchemaID,
					StateDataJson: fmt.Sprintf(`{
					   "salt": "%s",
					   "owner": "%s",
					   "amount": "%s"
					}`, types.RandHex(32), fromAddr, toKeep.Text(10)),
				})
			}
			if toAddr != nil && amount.Sign() > 0 {
				// Generate the coin to transfer
				newStates = append(newStates, &proto.StateData{
					SchemaId: fakeCoinSchemaID,
					StateDataJson: fmt.Sprintf(`{
					   "salt": "%s",
					   "owner": "%s",
					   "amount": "%s"
					}`, types.RandHex(32), toAddr, amount.Text(10)),
				})
			}
			return &proto.AssembleTransactionResponse{
				AssembledTransaction: &proto.AssembledTransaction{
					SpentStateIds: coinsToSpend,
					NewStates:     newStates,
				},
				AssemblyResult: proto.AssemblyResult_OK,
				AttestationPlan: []*proto.AttestationRequest{
					{
						Name:            "sender",
						AttestationType: proto.AttestationType_SIGN,
						Algorithm:       signer.Algorithm_ECDSA_SECP256K1_PLAINBYTES,
						Payload:         types.RandBytes(32), // TODO: eip712StatesWithSalt(newStates),
						Parties: []string{
							req.Transaction.From,
						},
					},
				},
			}, nil
		},

		PREPARE_TRANSACTION: func(_ simCallbacks, iReq pb.Message) (pb.Message, error) {
			req := iReq.(*proto.PrepareTransactionRequest)
			var signerSignature ethtypes.HexBytes0xPrefix
			for _, att := range req.AttestationResult {
				if att.AttestationType == proto.AttestationType_SIGN && att.Name == "sender" {
					signerSignature = att.Payload
				}
			}
			return &proto.PrepareTransactionResponse{
				Transaction: &proto.BaseLedgerTransaction{
					FunctionName: "executeNotarized",
					ParamsJson: toJSONString(t, map[string]interface{}{
						"txId":      req.Transaction.TransactionId,
						"inputs":    req.Transaction.SpentStateIds,
						"outputs":   req.Transaction.NewStateIds,
						"signature": signerSignature,
					}),
					SigningAddress: "domain1/contract1/notary",
				},
			}, nil
		},
	})
	defer done()

	err := rpcCall(&factoryAddr, "testbed_deployBytecode", "domain1_admin",
		mustParseBuildABI(simDomainBuild), mustParseBuildBytecode(simDomainBuild),
		types.RawJSON(`{}`)) // no params on constructor
	assert.NoError(t, err)

	err = rpcCall(types.RawJSON{}, "testbed_configureInit", "domain1", types.RawJSON(`{
		"some": "config"
	}`))
	assert.NoError(t, err)

	err = rpcCall(types.RawJSON{}, "testbed_configureInit", "domain1", types.RawJSON(`{
		"some": "config"
	}`))
	assert.NoError(t, err)

	var contractAddr ethtypes.Address0xHex
	err = rpcCall(&contractAddr, "testbed_deploy", "domain1", types.RawJSON(`{
		"notary": "domain1/contract1/notary",
		"name": "FakeToken1",
		"symbol": "FT1"
	}`))
	assert.NoError(t, err)

	err = rpcCall(types.RawJSON{}, "testbed_invoke", &types.PrivateContractInvoke{
		From:     "wallets/org1/aaaaaa",
		To:       types.EthAddress(contractAddr),
		Function: *mustParseABIEntry(fakeCoinTransferABI),
		Inputs:   types.RawJSON(fakeTransferMintPayload),
	})
	assert.NoError(t, err)

	err = rpcCall(types.RawJSON{}, "testbed_invoke", &types.PrivateContractInvoke{
		From:     "wallets/org1/aaaaaa",
		To:       types.EthAddress(contractAddr),
		Function: *mustParseABIEntry(fakeCoinTransferABI),
		Inputs:   types.RawJSON(fakeTransferPayload1),
	})
	assert.NoError(t, err)

}
