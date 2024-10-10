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

package registrymgr

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/kaleido-io/paladin/config/pkg/pldconf"
	"github.com/kaleido-io/paladin/toolkit/pkg/query"

	"github.com/kaleido-io/paladin/toolkit/pkg/plugintk"
	"github.com/kaleido-io/paladin/toolkit/pkg/prototk"
	"github.com/kaleido-io/paladin/toolkit/pkg/tktypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var registryID uuid.UUID

type testPlugin struct {
	plugintk.RegistryAPIBase
	initialized  atomic.Bool
	r            *registry
	sendMessages chan *prototk.SendMessageRequest
}

func (tp *testPlugin) Initialized() {
	tp.initialized.Store(true)
}

func newTestPlugin(registryFuncs *plugintk.RegistryAPIFunctions) *testPlugin {
	return &testPlugin{
		RegistryAPIBase: plugintk.RegistryAPIBase{
			Functions: registryFuncs,
		},
		sendMessages: make(chan *prototk.SendMessageRequest, 1),
	}
}

func newTestRegistry(t *testing.T, realDB bool, extraSetup ...func(mc *mockComponents, regConf *prototk.RegistryConfig)) (context.Context, *registryManager, *testPlugin, *mockComponents, func()) {
	regConf := &prototk.RegistryConfig{}

	ctx, rm, mc, done := newTestRegistryManager(t, realDB, &pldconf.RegistryManagerConfig{
		Registries: map[string]*pldconf.RegistryConfig{
			"test1": {
				Config: map[string]any{"some": "conf"},
			},
		},
	}, func(mc *mockComponents) {
		for _, fn := range extraSetup {
			fn(mc, regConf)
		}
	})

	tp := newTestPlugin(nil)
	tp.Functions = &plugintk.RegistryAPIFunctions{
		ConfigureRegistry: func(ctx context.Context, ctr *prototk.ConfigureRegistryRequest) (*prototk.ConfigureRegistryResponse, error) {
			assert.Equal(t, "test1", ctr.Name)
			assert.JSONEq(t, `{"some":"conf"}`, ctr.ConfigJson)
			return &prototk.ConfigureRegistryResponse{
				RegistryConfig: regConf,
			}, nil
		},
	}

	registerTestRegistry(t, rm, tp)
	return ctx, rm, tp, mc, done
}

func registerTestRegistry(t *testing.T, rm *registryManager, tp *testPlugin) {
	registryID = uuid.New()
	_, err := rm.RegistryRegistered("test1", registryID, tp)
	require.NoError(t, err)

	ra := rm.registriesByName["test1"]
	assert.NotNil(t, ra)
	tp.r = ra
	tp.r.initRetry.UTSetMaxAttempts(1)
	<-tp.r.initDone
}

func TestDoubleRegisterReplaces(t *testing.T) {

	_, rm, tp0, _, done := newTestRegistry(t, false)
	defer done()
	assert.Nil(t, tp0.r.initError.Load())
	assert.True(t, tp0.initialized.Load())

	// Register again
	tp1 := newTestPlugin(nil)
	tp1.Functions = tp0.Functions
	registerTestRegistry(t, rm, tp1)
	assert.Nil(t, tp1.r.initError.Load())
	assert.True(t, tp1.initialized.Load())

	// Check we get the second from all the maps
	byName := rm.registriesByName[tp1.r.name]
	assert.Same(t, tp1.r, byName)
	byUUID := rm.registriesByID[tp1.r.id]
	assert.Same(t, tp1.r, byUUID)

}

func TestUpsertTransportDetailsRealDBok(t *testing.T) {
	ctx, rm, tp, _, done := newTestRegistry(t, true)
	defer done()

	r, err := rm.GetRegistry(ctx, "test1")
	require.NoError(t, err)
	db := rm.persistence.DB()

	randID := func() string { return tktypes.RandHex(32) }
	randInt := func() int64 {
		i, _ := rand.Int(rand.Reader, big.NewInt(10^9))
		return i.Int64()
	}
	randChainInfo := func() *prototk.OnChainEventLocation {
		return &prototk.OnChainEventLocation{
			TransactionHash: tktypes.RandHex(32),
			BlockNumber:     randInt(), TransactionIndex: randInt(), LogIndex: randInt(),
		}
	}
	randPropFor := func(id string) *prototk.RegistryProperty {
		return &prototk.RegistryProperty{
			EntryId:  id,
			Name:     fmt.Sprintf("prop_%s", tktypes.RandHex(5)),
			Value:    fmt.Sprintf("val_%s", tktypes.RandHex(5)),
			Active:   true,
			Location: randChainInfo(),
		}
	}

	// Insert a root entry
	rootEntry1 := &prototk.RegistryEntry{Id: randID(), Name: "entry1", Location: randChainInfo(), Active: true}
	rootEntry1Props1 := randPropFor(rootEntry1.Id)
	rootEntry2 := &prototk.RegistryEntry{Id: randID(), Name: "entry2", Location: randChainInfo(), Active: true}
	rootEntry2Props1 := randPropFor(rootEntry2.Id)
	rootEntry2Props2 := randPropFor(rootEntry2.Id)
	upsert1 := &prototk.UpsertRegistryRecordsRequest{
		Entries:    []*prototk.RegistryEntry{rootEntry1, rootEntry2},
		Properties: []*prototk.RegistryProperty{rootEntry1Props1, rootEntry2Props1, rootEntry2Props2},
	}

	// Upsert first entry
	res, err := tp.r.UpsertRegistryRecords(ctx, upsert1)
	require.NoError(t, err)
	assert.NotNil(t, res)

	// Test getting all the entries with props
	entries, err := r.QueryEntriesWithProps(ctx, db, "active", query.NewQueryBuilder().Query())
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, rootEntry1.Id, entries[0].ID.HexString())
	require.Len(t, entries[0].Properties, 1)
	require.Equal(t, rootEntry1Props1.Value, entries[0].Properties[rootEntry1Props1.Name])
	assert.Equal(t, rootEntry2.Id, entries[1].ID.HexString())
	require.Len(t, entries[1].Properties, 2)
	require.Equal(t, rootEntry2Props1.Value, entries[1].Properties[rootEntry2Props1.Name])
	require.Equal(t, rootEntry2Props2.Value, entries[1].Properties[rootEntry2Props2.Name])

	// Test on a non-null field
	entries, err = r.QueryEntriesWithProps(ctx, db, "active",
		query.NewQueryBuilder().NotNull(rootEntry2Props2.Name).Query(),
	)
	require.NoError(t, err)
	assert.Equal(t, rootEntry2.Id, entries[0].ID.HexString())
	require.Len(t, entries, 1)
	require.Len(t, entries[0].Properties, 2)
	require.Equal(t, rootEntry2Props1.Value, entries[0].Properties[rootEntry2Props1.Name])
	require.Equal(t, rootEntry2Props2.Value, entries[0].Properties[rootEntry2Props2.Name])

	// Test on an equal field
	entries, err = r.QueryEntriesWithProps(ctx, db, "active",
		query.NewQueryBuilder().Equal(rootEntry1Props1.Name, rootEntry1Props1.Value).Query(),
	)
	require.NoError(t, err)
	assert.Equal(t, rootEntry1.Id, entries[0].ID.HexString())
	require.Len(t, entries, 1)
	require.Len(t, entries[0].Properties, 1)
	require.Equal(t, rootEntry1Props1.Value, entries[0].Properties[rootEntry1Props1.Name])

	// Add a child entry - checking it's allowed to have the same name without replacing
	root1ChildEntry1 := &prototk.RegistryEntry{Id: randID(), Name: "entry1", ParentId: rootEntry1.Id, Location: randChainInfo(), Active: true}
	res, err = tp.r.UpsertRegistryRecords(ctx, &prototk.UpsertRegistryRecordsRequest{
		Entries: []*prototk.RegistryEntry{root1ChildEntry1},
	})
	require.NoError(t, err)
	assert.NotNil(t, res)

	// Find children and check sorting fields
	children, err := r.QueryEntries(ctx, db, "active", query.NewQueryBuilder().Equal(
		".parentId", rootEntry1.Id,
	).Sort("-.created", "-.updated").Query())
	require.NoError(t, err)
	require.Len(t, children, 1)
	require.Equal(t, root1ChildEntry1.Id, children[0].ID.HexString())

	// Make an entry inactive - this does NOT affect child entries (responsibility
	// is on the registry plugin to do this if it wishes).
	rootEntry2.Active = false                      // make entry inactive
	rootEntry2Props2.Active = false                // make one prop inactive
	rootEntry2Props3 := randPropFor(rootEntry2.Id) // add prop as active
	res, err = tp.r.UpsertRegistryRecords(ctx, &prototk.UpsertRegistryRecordsRequest{
		Entries:    []*prototk.RegistryEntry{rootEntry2},
		Properties: []*prototk.RegistryProperty{rootEntry2Props2, rootEntry2Props3},
	})
	require.NoError(t, err)
	assert.NotNil(t, res)

	// Check not returned from normal query
	entries, err = r.QueryEntriesWithProps(ctx, db, "active",
		query.NewQueryBuilder().Null(".parentId").Query())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, rootEntry1.Id, entries[0].ID.HexString())

	// Check returned from cherry pick with any
	entries, err = r.QueryEntriesWithProps(ctx, db, "any",
		query.NewQueryBuilder().Equal(".name", rootEntry2.Name).Query())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, rootEntry2.Id, entries[0].ID.HexString())
	assert.False(t, entries[0].Active)
	// ... but here the props are the active props still (prop 2 excluded)
	require.Len(t, entries[0].Properties, 2)
	require.Equal(t, rootEntry2Props1.Value, entries[0].Properties[rootEntry2Props1.Name])
	require.Equal(t, rootEntry2Props3.Value, entries[0].Properties[rootEntry2Props3.Name])

	// Check returned from cherry pick with inactive
	entries, err = r.QueryEntriesWithProps(ctx, db, "inactive",
		query.NewQueryBuilder().Equal(".id", rootEntry2.Id).Query())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, rootEntry2.Id, entries[0].ID.HexString())

	// Can get the complete prop set
	allProps, err := r.GetEntryProperties(ctx, db, "any", tktypes.MustParseHexBytes(rootEntry2.Id))
	require.NoError(t, err)
	propsMap := filteredPropsMap(allProps, tktypes.MustParseHexBytes(rootEntry2.Id))
	require.Len(t, propsMap, 3)
	require.Equal(t, rootEntry2Props1.Value, propsMap[rootEntry2Props1.Name])
	require.Equal(t, rootEntry2Props2.Value, propsMap[rootEntry2Props2.Name])
	require.Equal(t, rootEntry2Props3.Value, propsMap[rootEntry2Props3.Name])

	// Can get just the inactive props set
	allProps, err = r.GetEntryProperties(ctx, db, "inactive", tktypes.MustParseHexBytes(rootEntry2.Id))
	require.NoError(t, err)
	propsMap = filteredPropsMap(allProps, tktypes.MustParseHexBytes(rootEntry2.Id))
	require.Len(t, propsMap, 1)
	require.Equal(t, rootEntry2Props2.Value, propsMap[rootEntry2Props2.Name])
}

// func TestUpsertTransportDetailsInsertFail(t *testing.T) {
// 	ctx, _, tp, m, done := newTestRegistry(t, false)
// 	defer done()

// 	m.db.ExpectBegin()
// 	m.db.ExpectExec("INSERT.*registry_transport_details").WillReturnError(fmt.Errorf("pop"))

// 	_, err := tp.r.UpsertTransportDetails(ctx, &prototk.UpsertTransportDetails{
// 		TransportDetails: []*prototk.TransportDetails{
// 			{
// 				Node:      "node1",
// 				Transport: "websockets",
// 				Details:   "more things and stuff",
// 			},
// 		},
// 	})
// 	assert.Regexp(t, "pop", err)

// }

// func TestGetNodeTransportsCache(t *testing.T) {
// 	ctx, rm, _, m, done := newTestRegistry(t, false)
// 	defer done()

// 	m.db.ExpectQuery("SELECT.*registry_transport_details").WillReturnRows(sqlmock.NewRows([]string{
// 		"node", "registry", "transport", "details",
// 	}).AddRow(
// 		"node1", "test1", "websockets", "things and stuff",
// 	))

// 	expected := []*components.RegistryNodeTransportEntry{
// 		{
// 			Node:      "node1",
// 			Registry:  "test1",
// 			Transport: "websockets",
// 			Details:   "things and stuff",
// 		},
// 	}

// 	transports, err := rm.GetNodeTransports(ctx, "node1")
// 	require.NoError(t, err)
// 	assert.Equal(t, expected, transports)

// 	// Re-do from cache
// 	transports, err = rm.GetNodeTransports(ctx, "node1")
// 	require.NoError(t, err)
// 	assert.Equal(t, expected, transports)

// }

// func TestGetNodeTransportsErr(t *testing.T) {
// 	ctx, rm, _, m, done := newTestRegistry(t, false)
// 	defer done()

// 	m.db.ExpectQuery("SELECT.*registry_transport_details").WillReturnError(fmt.Errorf("pop"))

// 	_, err := rm.GetNodeTransports(ctx, "node1")
// 	require.Regexp(t, "pop", err)
// }

// func TestRegistryWithEventStreams(t *testing.T) {
// 	es := &blockindexer.EventStream{ID: uuid.New()}

// 	_, _, tp, _, done := newTestRegistry(t, false, func(mc *mockComponents, regConf *prototk.RegistryConfig) {
// 		a := abi.ABI{
// 			{
// 				Type: abi.Event,
// 				Name: "Registered",
// 				Inputs: abi.ParameterArray{
// 					{Name: "node", Type: "string"},
// 					{Name: "details", Type: "string"},
// 				},
// 			},
// 		}
// 		addr := tktypes.RandAddress()

// 		mc.blockIndexer.On("AddEventStream", mock.Anything, mock.MatchedBy(func(ies *blockindexer.InternalEventStream) bool {
// 			require.Len(t, ies.Definition.Sources, 1)
// 			assert.JSONEq(t, tktypes.JSONString(a).String(), tktypes.JSONString(ies.Definition.Sources[0].ABI).String())
// 			assert.Equal(t, addr, ies.Definition.Sources[0].Address)
// 			return true
// 		})).Return(es, nil)

// 		regConf.EventSources = []*prototk.RegistryEventSource{
// 			{
// 				ContractAddress: addr.String(),
// 				AbiEventsJson:   tktypes.JSONString(a).Pretty(),
// 			},
// 		}
// 	})
// 	defer done()

// 	assert.Equal(t, es, tp.r.eventStream)

// }

// func TestConfigureEventStreamBadEventABI(t *testing.T) {
// 	ctx, _, tp, _, done := newTestRegistry(t, false)
// 	defer done()

// 	tp.r.config = &prototk.RegistryConfig{
// 		EventSources: []*prototk.RegistryEventSource{
// 			{
// 				AbiEventsJson: `{!!! wrong `,
// 			},
// 		},
// 	}
// 	err := tp.r.configureEventStream(ctx)
// 	assert.Regexp(t, "PD012103", err)

// }

// func TestConfigureEventStreamBadEventContractAddr(t *testing.T) {
// 	ctx, _, tp, _, done := newTestRegistry(t, false)
// 	defer done()

// 	tp.r.config = &prototk.RegistryConfig{
// 		EventSources: []*prototk.RegistryEventSource{
// 			{
// 				ContractAddress: "wrong",
// 			},
// 		},
// 	}
// 	err := tp.r.configureEventStream(ctx)
// 	assert.Regexp(t, "PD012103", err)

// }

// func TestConfigureEventStreamBadEventABITypes(t *testing.T) {
// 	ctx, _, tp, _, done := newTestRegistry(t, false)
// 	defer done()

// 	tp.r.config = &prototk.RegistryConfig{
// 		EventSources: []*prototk.RegistryEventSource{
// 			{
// 				AbiEventsJson: `[{"type":"event","inputs":[{"type":"badness"}]}]`,
// 			},
// 		},
// 	}
// 	err := tp.r.configureEventStream(ctx)
// 	assert.Regexp(t, "FF22025", err)

// }

// func TestHandleEventBatchOk(t *testing.T) {

// 	ctx, _, tp, _, done := newTestRegistry(t, false, func(mc *mockComponents, regConf *prototk.RegistryConfig) {
// 		mc.db.ExpectExec("INSERT.*registry_transport_details").WillReturnResult(driver.ResultNoRows)
// 	})
// 	defer done()

// 	batch := &blockindexer.EventDeliveryBatch{
// 		StreamID:   uuid.New(),
// 		StreamName: "registry_1",
// 		BatchID:    uuid.New(),
// 		Events: []*blockindexer.EventWithData{
// 			{
// 				IndexedEvent: &blockindexer.IndexedEvent{
// 					BlockNumber:      12345,
// 					TransactionIndex: 10,
// 					LogIndex:         20,
// 					TransactionHash:  tktypes.Bytes32(tktypes.RandBytes(32)),
// 					Signature:        tktypes.Bytes32(tktypes.RandBytes(32)),
// 				},
// 				SoliditySignature: "event1()",
// 				Address:           *tktypes.RandAddress(),
// 				Data:              []byte("some data"),
// 			},
// 		},
// 	}

// 	tp.Functions.RegistryEventBatch = func(ctx context.Context, rebr *prototk.RegistryEventBatchRequest) (*prototk.RegistryEventBatchResponse, error) {
// 		assert.Equal(t, batch.BatchID.String(), rebr.BatchId)
// 		assert.Equal(t, "event1()", rebr.Events[0].SoliditySignature)
// 		return &prototk.RegistryEventBatchResponse{
// 			TransportDetails: []*prototk.TransportDetails{
// 				{
// 					Node:      "node1",
// 					Transport: "websockets",
// 					Details:   "some details",
// 				},
// 			},
// 		}, nil
// 	}

// 	res, err := tp.r.handleEventBatch(ctx, tp.r.rm.persistence.DB(), batch)
// 	require.NoError(t, err)
// 	assert.NotNil(t, res)

// }

// func TestHandleEventBatchError(t *testing.T) {

// 	ctx, _, tp, _, done := newTestRegistry(t, false)
// 	defer done()

// 	batch := &blockindexer.EventDeliveryBatch{
// 		BatchID: uuid.New(),
// 		Events: []*blockindexer.EventWithData{{
// 			IndexedEvent: &blockindexer.IndexedEvent{
// 				BlockNumber:      12345,
// 				TransactionIndex: 10,
// 				LogIndex:         20,
// 				TransactionHash:  tktypes.Bytes32(tktypes.RandBytes(32)),
// 				Signature:        tktypes.Bytes32(tktypes.RandBytes(32)),
// 			},
// 			SoliditySignature: "event1()",
// 			Address:           *tktypes.RandAddress(),
// 			Data:              []byte("some data"),
// 		}},
// 	}

// 	tp.Functions.RegistryEventBatch = func(ctx context.Context, rebr *prototk.RegistryEventBatchRequest) (*prototk.RegistryEventBatchResponse, error) {
// 		return nil, fmt.Errorf("pop")
// 	}

// 	_, err := tp.r.handleEventBatch(ctx, tp.r.rm.persistence.DB(), batch)
// 	require.Regexp(t, "pop", err)

// }
