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

package publictxmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/hyperledger/firefly-common/pkg/fftypes"
	"github.com/hyperledger/firefly-common/pkg/i18n"
	"github.com/hyperledger/firefly-signer/pkg/ethsigner"
	"github.com/hyperledger/firefly-signer/pkg/ethtypes"
	"github.com/kaleido-io/paladin/core/internal/components"
	"github.com/kaleido-io/paladin/core/internal/filters"
	"github.com/kaleido-io/paladin/core/pkg/blockindexer"
	"github.com/kaleido-io/paladin/core/pkg/ethclient"
	"github.com/kaleido-io/paladin/core/pkg/persistence"
	"github.com/kaleido-io/paladin/toolkit/pkg/algorithms"
	"github.com/kaleido-io/paladin/toolkit/pkg/confutil"
	"github.com/kaleido-io/paladin/toolkit/pkg/log"
	"github.com/kaleido-io/paladin/toolkit/pkg/ptxapi"
	"github.com/kaleido-io/paladin/toolkit/pkg/query"
	"github.com/kaleido-io/paladin/toolkit/pkg/retry"
	"github.com/kaleido-io/paladin/toolkit/pkg/tktypes"

	"github.com/kaleido-io/paladin/core/internal/msgs"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// configurations
// metrics

// UpdateType informs policy loop whether the transaction needs an update to be persisted after a transaction processor finished processing a transaction
type UpdateType int

const (
	UpdateNo     UpdateType = iota // Instructs that no update is necessary
	UpdateYes                      // Instructs that the transaction should be updated in persistence
	UpdateDelete                   // Instructs that the transaction should be removed completely from persistence - generally only returned when TX status is TxStatusDeleteRequested
)

// Public Tx Engine:
// - It offers two ways of calculating gas price: use a fixed number, use the built-in API of a ethereum connector
// - It resubmits the transaction based on a configured interval until it succeed or fail
// - It also recalculate gas price during resubmissions
// - It logs errors transactions breach certain configured thresholds of staleness
// - It offers caches of gas price for transactions targeting same method of a smart contract
// - It provide a outbound request concurrency control

type pubTxManager struct {
	ctx       context.Context
	ctxCancel context.CancelFunc

	conf      *Config
	thMetrics *publicTxEngineMetrics
	p         persistence.Persistence
	bIndexer  blockindexer.BlockIndexer
	ethClient ethclient.EthClient
	keymgr    ethclient.KeyManager
	rootTxMgr components.TXManager
	// gas price
	gasPriceClient   GasPriceClient
	submissionWriter *submissionWriter

	// nonce manager
	nonceManager NonceCache

	// a map of signing addresses and transaction engines
	InFlightOrchestrators       map[tktypes.EthAddress]*orchestrator
	SigningAddressesPausedUntil map[tktypes.EthAddress]time.Time
	InFlightOrchestratorMux     sync.Mutex
	InFlightOrchestratorStale   chan bool

	// inbound concurrency control TBD

	// engine config
	maxInFlightOrchestrators int
	maxOrchestratorStale     time.Duration
	maxOrchestratorIdle      time.Duration
	maxOverloadProcessTime   time.Duration
	retry                    *retry.Retry
	enginePollingInterval    time.Duration
	engineLoopDone           chan struct{}

	// balance manager
	balanceManager BalanceManager

	// orchestrator config
	gasPriceIncreaseMax     *big.Int
	gasPriceIncreasePercent int
}

func NewPublicTransactionManager(ctx context.Context, conf *Config) components.PublicTxManager {
	log.L(ctx).Debugf("Creating new enterprise transaction handler")

	gasPriceClient := NewGasPriceClient(ctx, conf)
	gasPriceIncreaseMax := confutil.BigIntOrNil(conf.GasPrice.IncreaseMax)

	log.L(ctx).Debugf("Enterprise transaction handler created")

	ptmCtx, ptmCtxCancel := context.WithCancel(log.WithLogField(ctx, "role", "public_tx_mgr"))

	return &pubTxManager{
		ctx:                         ptmCtx,
		ctxCancel:                   ptmCtxCancel,
		gasPriceClient:              gasPriceClient,
		InFlightOrchestratorStale:   make(chan bool, 1),
		SigningAddressesPausedUntil: make(map[tktypes.EthAddress]time.Time),
		maxInFlightOrchestrators:    confutil.IntMin(conf.Orchestrator.MaxInFlight, 1, *DefaultConfig.Orchestrator.MaxInFlight),
		maxOverloadProcessTime:      confutil.DurationMin(conf.TransactionEngine.MaxOverloadProcessTime, 0, *DefaultConfig.TransactionEngine.MaxOverloadProcessTime),
		maxOrchestratorStale:        confutil.DurationMin(conf.TransactionEngine.MaxStaleTime, 0, *DefaultConfig.TransactionEngine.MaxStaleTime),
		maxOrchestratorIdle:         confutil.DurationMin(conf.TransactionEngine.MaxIdleTime, 0, *DefaultConfig.TransactionEngine.MaxIdleTime),
		enginePollingInterval:       confutil.DurationMin(conf.TransactionEngine.Interval, 50*time.Millisecond, *conf.TransactionEngine.Interval),
		retry:                       retry.NewRetryIndefinite(&conf.TransactionEngine.Retry),
		gasPriceIncreaseMax:         gasPriceIncreaseMax,
		gasPriceIncreasePercent:     confutil.Int(conf.GasPrice.IncreasePercentage, *DefaultConfig.GasPrice.IncreasePercentage),
	}
}

// Post-init allows the manager to cross-bind to other components, or the Engine
func (ble *pubTxManager) PostInit(components.AllComponents) error {
	return nil
}

func (ble *pubTxManager) PreInit(pic components.PreInitComponents) (result *components.ManagerInitResult, err error) {
	ctx := ble.ctx
	log.L(ctx).Debugf("Initializing enterprise transaction handler")
	ble.ethClient = pic.EthClientFactory().SharedWS()
	ble.keymgr = pic.KeyManager()
	ble.gasPriceClient.Init(ctx, ble.ethClient)
	ble.bIndexer = pic.BlockIndexer()
	ble.nonceManager = newNonceCache(1*time.Hour, func(ctx context.Context, signer tktypes.EthAddress) (uint64, error) {
		log.L(ctx).Tracef("NonceFromChain getting next nonce for signing address ID %s", signer)
		nextNonce, err := ble.ethClient.GetTransactionCount(ctx, signer)
		if err != nil {
			log.L(ctx).Errorf("NonceFromChain getting next nonce for signer %s failed: %+v", signer, err)
			return 0, err
		}
		log.L(ctx).Tracef("NonceFromChain getting next nonce for signer %s succeeded: %s, converting to uint: %d", signer, nextNonce.String(), nextNonce.Uint64())
		return nextNonce.Uint64(), nil
	})

	balanceManager, err := NewBalanceManagerWithInMemoryTracking(ctx, ble.conf, ble.ethClient, ble)
	if err != nil {
		log.L(ctx).Errorf("Failed to create balance manager for enterprise transaction handler due to %+v", err)
		panic(err)
	}
	log.L(ctx).Debugf("Initialized enterprise transaction handler")
	ble.balanceManager = balanceManager
	ble.submissionWriter = newSubmissionWriter(ctx, ble.p, ble.conf)
	return &components.ManagerInitResult{}, nil
}

func (ble *pubTxManager) Start() error {
	ctx := ble.ctx
	log.L(ctx).Debugf("Starting enterprise transaction handler")
	if ble.ctx == nil { // only start once
		ble.ctx = ctx // set the context for policy loop
		ble.engineLoopDone = make(chan struct{})
		log.L(ctx).Debugf("Kicking off  enterprise handler engine loop")
		go ble.engineLoop()
	}
	ble.MarkInFlightOrchestratorsStale()
	log.L(ctx).Infof("Started enterprise transaction handler")
	return nil
}

func (ble *pubTxManager) Stop() {
	ble.submissionWriter.Shutdown()
	ble.nonceManager.Stop()
	ble.ctxCancel()
	<-ble.engineLoopDone
}

type preparedTransaction struct {
	tx          *ptxapi.PublicTxWithID
	keyHandle   string
	rejectError error                 // only if rejected
	revertData  tktypes.HexBytes      // only if rejected, and was available
	nsi         NonceAssignmentIntent // only if accepted
}

type preparedTransactionBatch struct {
	ble      *pubTxManager
	accepted []components.PublicTxAccepted
	rejected []components.PublicTxRejected
}

// Submit writes the prepared submission to the database using the provided context
// This is expected to be a lightweight operation involving not much more than writing to the database, as the heavy lifting should have been done in PrepareSubmission
// The database transaction will be coordinated by the caller
func (pb *preparedTransactionBatch) Submit(ctx context.Context, dbTX *gorm.DB) (err error) {
	persistedTransactions := make([]*persistedPubTx, len(pb.accepted))
	for i, accepted := range pb.accepted {
		ptx := accepted.(*preparedTransaction)
		persistedTransactions[i], err = pb.ble.finalizeNonceForPersistedTX(ctx, ptx)
		if err != nil {
			return err
		}
	}
	// All the nonce processing to this point should have ensured we do not have a conflict on nonces.
	// It is the caller's responsibility to ensure we do not have a conflict on transaction+resubmit_idx.
	return dbTX.
		WithContext(ctx).
		Table("public_tx").
		Create(persistedTransactions).
		Error
}

func (pb *preparedTransactionBatch) Accepted() []components.PublicTxAccepted { return pb.accepted }
func (pb *preparedTransactionBatch) Rejected() []components.PublicTxRejected { return pb.rejected }

func (pb *preparedTransactionBatch) Completed(ctx context.Context, committed bool) {
	for _, pt := range pb.accepted {
		if !committed {
			pt.(*preparedTransaction).nsi.Rollback(ctx)
		}
	}
	if committed && len(pb.accepted) > 0 {
		log.L(ctx).Debugf("%d transactions committed to DB", len(pb.accepted))
		pb.ble.MarkInFlightOrchestratorsStale()
	}
}

func (pt *preparedTransaction) TXID() *ptxapi.PublicTxID {
	return &pt.tx.PublicTxID
}

func (pt *preparedTransaction) TXWithNonce() *ptxapi.PublicTxWithID {
	return pt.tx
}

func (pt *preparedTransaction) RejectedError() error {
	return pt.rejectError
}

func (pt *preparedTransaction) RevertData() tktypes.HexBytes {
	return pt.revertData
}

func (ble *pubTxManager) PrepareSubmissionBatch(ctx context.Context, transactions []*components.PublicTxIDInput) (components.PublicTxBatch, error) {
	batch := &preparedTransactionBatch{
		ble:      ble,
		accepted: make([]components.PublicTxAccepted, 0, len(transactions)),
		rejected: make([]components.PublicTxRejected, 0),
	}
	earlyReturn := true
	defer func() {
		if earlyReturn {
			// Ensure we always cleanup if we fail (for error or panic) before we've
			// delegated responsibility for calling this to our caller
			batch.Completed(ctx, false)
		}
	}()
	for _, tx := range transactions {
		preparedSubmission, err := ble.prepareSubmission(ctx, tx)
		if err != nil {
			return nil, err
		}
		if preparedSubmission.rejectError != nil {
			batch.rejected = append(batch.rejected, preparedSubmission)
		} else {
			batch.accepted = append(batch.accepted, preparedSubmission)
		}
	}
	earlyReturn = false
	return batch, nil
}

// A one-and-done submission of a single transaction, used internally by auto-fueling, and demonstrating use of the
// public transaction interface for the special case of a single transaction that will succeed or fail.
// Other callers have to handle the Accepted()/Rejected() list to decide what they do for a split result.
func (ble *pubTxManager) SingleTransactionSubmit(ctx context.Context, transaction *components.PublicTxIDInput) (components.PublicTxAccepted, error) {
	batch, err := ble.PrepareSubmissionBatch(ctx, []*components.PublicTxIDInput{transaction})
	if err != nil {
		return nil, err
	}
	// Must call completed and tell it whether the allocation of the nonces committed or rolled back
	committed := false
	defer func() {
		batch.Completed(ctx, committed)
	}()
	// Try to submit
	if len(batch.Rejected()) > 0 {
		return nil, batch.Rejected()[0].RejectedError()
	}
	err = ble.p.DB().Transaction(func(dbTX *gorm.DB) error {
		return batch.Submit(ctx, dbTX)
	})
	if err != nil {
		return nil, err
	}
	// We committed - so the nonces are finalized as allocated
	committed = true
	return batch.Accepted()[0], nil
}

func buildEthTX(
	from tktypes.EthAddress,
	nonce *uint64,
	to *tktypes.EthAddress,
	data tktypes.HexBytes,
	options *ptxapi.PublicTxOptions,
) *ethsigner.Transaction {
	ethTx := &ethsigner.Transaction{
		From:                 json.RawMessage(tktypes.JSONString(from)),
		To:                   to.Address0xHex(),
		GasPrice:             (*ethtypes.HexInteger)(options.GasPrice),
		MaxPriorityFeePerGas: (*ethtypes.HexInteger)(options.MaxPriorityFeePerGas),
		MaxFeePerGas:         (*ethtypes.HexInteger)(options.MaxFeePerGas),
		Value:                (*ethtypes.HexInteger)(options.Value),
		Data:                 ethtypes.HexBytes0xPrefix(data),
	}
	if nonce != nil {
		ethTx.Nonce = ethtypes.NewHexIntegerU64(*nonce)
	}
	if options.Gas != nil {
		ethTx.GasLimit = ethtypes.NewHexIntegerU64(options.Gas.Uint64())
	}
	return ethTx
}

// PrepareSubmission prepares and validates the transaction input data so that a later call to
// Submit can be made in the middle of a wider database transaction with minimal risk of error
func (ble *pubTxManager) prepareSubmission(ctx context.Context, txi *components.PublicTxIDInput) (preparedSubmission *preparedTransaction, err error) {
	log.L(ctx).Tracef("PrepareSubmission transaction: %+v", txi)

	pt := &preparedTransaction{
		tx: &ptxapi.PublicTxWithID{
			PublicTxID: txi.PublicTxID,
			PublicTx: ptxapi.PublicTx{
				To:              txi.To,
				Data:            txi.Data,
				PublicTxOptions: txi.PublicTxOptions,
			},
		},
	}

	var fromAddr *tktypes.EthAddress
	keyHandle, fromAddrString, err := ble.keymgr.ResolveKey(ctx, txi.From, algorithms.ECDSA_SECP256K1_PLAINBYTES)
	if err == nil {
		fromAddr, err = tktypes.ParseEthAddress(fromAddrString)
	}
	if err != nil {
		return nil, err
	}
	pt.keyHandle = keyHandle
	pt.tx.From = *fromAddr

	prepareStart := time.Now()
	var txType InFlightTxOperation

	rejected := false
	if pt.tx.Gas == nil || *pt.tx.Gas == 0 {
		gasEstimateResult, err := ble.ethClient.EstimateGasNoResolve(ctx, buildEthTX(
			*fromAddr,
			nil, /* nonce not assigned at this point */
			pt.tx.To,
			pt.tx.Data,
			&pt.tx.PublicTxOptions,
		))
		if err != nil {
			log.L(ctx).Errorf("HandleNewTx <%s> error estimating gas for transaction: %+v, request: (%+v)", txType, err, pt.tx)
			ble.thMetrics.RecordOperationMetrics(ctx, string(txType), string(GenericStatusFail), time.Since(prepareStart).Seconds())
			if ethclient.MapSubmissionRejected(err) {
				// transaction is rejected, so no nonce will be assigned - but we have not failed in our task
				pt.rejectError = err
				// TODO: we pass the revert data back currently, but we probably should use the dictionary service
				// in the transaction manager to resolve this error to something friendly.
				// Or we need to explicitly decide we are pushing that back to our caller.
				pt.revertData = gasEstimateResult.RevertData
				return pt, nil
			}
			return nil, err
		}
		pt.tx.Gas = &gasEstimateResult.GasLimit
		log.L(ctx).Tracef("HandleNewTx <%s> using the estimated gas limit %s for transaction: %+v", txType, pt.tx.Gas, pt.tx)
	} else {
		log.L(ctx).Tracef("HandleNewTx <%s> using the provided gas limit %s for transaction: %+v", txType, pt.tx.Gas, pt.tx)
	}

	if !rejected {
		pt.nsi, err = ble.nonceManager.IntentToAssignNonce(ctx, pt.tx.From)
		if err != nil {
			log.L(ctx).Errorf("HandleNewTx <%s> error assigning nonce for transaction: %+v, request: (%+v)", txType, err, pt.tx)
			ble.thMetrics.RecordOperationMetrics(ctx, string(txType), string(GenericStatusFail), time.Since(prepareStart).Seconds())
			return nil, err
		}
	}

	ble.thMetrics.RecordOperationMetrics(ctx, string(txType), string(GenericStatusSuccess), time.Since(prepareStart).Seconds())
	log.L(ctx).Debugf("HandleNewTx <%s> transaction validated and nonce assignment intent created for %s", txType, pt.tx.From)
	return pt, nil

}

func (ble *pubTxManager) finalizeNonceForPersistedTX(ctx context.Context, ptx *preparedTransaction) (*persistedPubTx, error) {
	nonce, err := ptx.nsi.AssignNextNonce(ctx)
	if err != nil {
		log.L(ctx).Errorf("Failed to assign nonce to public transaction %+v: %s", ptx, err)
		return nil, err
	}
	tx := ptx.tx
	tx.Nonce = tktypes.HexUint64(nonce)
	log.L(ctx).Infof("Creating a new public transaction %s [%d] from=%s nonce=%d (%s)", tx.Transaction, tx.ResubmitIndex, tx.From, tx.Nonce /* number */, tx.Nonce /* hex */)
	log.L(ctx).Tracef("payload: %+v", tx)
	return &persistedPubTx{
		SignerNonce:   fmt.Sprintf("%s:%d", tx.From, tx.Nonce), // having a single key rather than compound key helps us simplify cross-table correlation, particularly for batch lookup
		From:          tx.From,
		Nonce:         tx.Nonce.Uint64(),
		KeyHandle:     ptx.keyHandle, // TODO: Consider once we have reverse mapping in key manager whether we still need this
		Transaction:   tx.Transaction,
		ResubmitIndex: tx.ResubmitIndex,
		To:            tx.To,
		Gas:           tx.Gas.Uint64(),
	}, nil
}

// HandleConfirmedTransactions
// handover events to the inflight orchestrators for the related signing addresses and record the highest confirmed nonce
// new orchestrators will be created if there are space, orchestrators will use the recorded highest nonce to drive completion logic of transactions
func (ble *pubTxManager) HandleConfirmedTransactions(ctx context.Context, confirmedTransactions []*blockindexer.IndexedTransaction) error {
	// firstly, we group the confirmed transactions by from address
	// note: filter out transactions that are before the recorded nonce in confirmedTXNonce map requires multiple reads to a single address (as the loop keep switching between addresses)
	// so we delegate the logic to the orchestrator as it will have a list of records for a single address
	itMap := make(map[tktypes.EthAddress]map[uint64]*blockindexer.IndexedTransaction)
	itMaxNonce := make(map[tktypes.EthAddress]uint64)
	for _, it := range confirmedTransactions {
		if itMap[*it.From] == nil {
			itMap[*it.From] = map[uint64]*blockindexer.IndexedTransaction{it.Nonce: it}
		} else {
			itMap[*it.From][it.Nonce] = it
		}
		if itMaxNonce[*it.From] < it.Nonce {
			itMaxNonce[*it.From] = it.Nonce
		}
	}
	if len(itMap) > 0 {
		// secondly, we obtain the lock for the orchestrator map:
		ble.InFlightOrchestratorMux.Lock()
		defer ble.InFlightOrchestratorMux.Unlock() // note, using lock might cause the event sequence to get lost when this function is invoked concurrently by several go routines, this code assumes the upstream logic does not do that

		//     for address that has or could have a running orchestrator, triggers event handlers of each orchestrator in parallel to handle the event
		//         (logic implemented in orchestrator handler)for the orchestrator handler, it obtains the stage process buffer lock and add the event into the stage process buffer and then exit

		localRWLock := sync.RWMutex{} // could consider switch InFlightOrchestrators to use sync.Map for this logic here as the go routines will only modify disjoint set of keys
		eventHandlingErrors := make(chan error, len(itMap))
		for from, its := range itMap {
			fromAddress := from
			indexedTxs := its
			go func() {
				localRWLock.RLock()
				inFlightOrchestrator := ble.InFlightOrchestrators[fromAddress]
				localRWLock.RUnlock()
				if inFlightOrchestrator == nil {
					localRWLock.Lock()
					itTotal := len(ble.InFlightOrchestrators)
					if itTotal < ble.maxInFlightOrchestrators {
						inFlightOrchestrator = NewOrchestrator(ble, fromAddress, ble.conf)
						ble.InFlightOrchestrators[fromAddress] = inFlightOrchestrator
						_, _ = inFlightOrchestrator.Start(ble.ctx)
						log.L(ctx).Infof("(Event handler) Engine added orchestrator for signing address %s", fromAddress)
						localRWLock.Unlock()
					} else {
						// no action can be taken
						log.L(ctx).Debugf("(Event handler) Cannot add orchestrator for signing address %s due to in-flight queue is full", fromAddress)
						localRWLock.Unlock()
						eventHandlingErrors <- nil
						return
					}
				}
				err := inFlightOrchestrator.HandleConfirmedTransactions(ctx, indexedTxs, itMaxNonce[fromAddress])
				// finally, we update the confirmed nonce for each address to the highest number that is observed ever. This then can be used by the orchestrator to retrospectively fetch missed confirmed transaction data.
				ble.notifyConfirmedTxNonce(fromAddress, itMaxNonce[fromAddress])
				eventHandlingErrors <- err
			}()
		}

		resultCount := 0
		var accumulatedError error

		// wait for all add output to complete
		for {
			select {
			case err := <-eventHandlingErrors:
				if err != nil {
					accumulatedError = err
				}
				resultCount++
			case <-ctx.Done():
				return i18n.NewError(ctx, msgs.MsgContextCanceled)
			}
			if resultCount == len(itMap) {
				break
			}
		}
		return accumulatedError
	}
	return nil
}

func recoverGasPriceOptions(gpoJSON tktypes.RawJSON) (ptgp ptxapi.PublicTxGasPricing) {
	if gpoJSON != nil {
		_ = json.Unmarshal(gpoJSON, &ptgp)
	}
	return
}

func (ble *pubTxManager) GetTransactions(ctx context.Context, dbTX *gorm.DB, jq *query.QueryJSON) ([]*ptxapi.PublicTxWithID, error) {
	q := filters.BuildGORM(ctx, jq, dbTX.Table("public_txns").WithContext(ctx), components.PublicTxFilterFields)
	ptxs, err := ble.runTransactionQuery(ctx, dbTX, q)
	if err != nil {
		return nil, err
	}
	results := make([]*ptxapi.PublicTxWithID, len(ptxs))
	for iTx, ptx := range ptxs {
		tx := mapPersistedTransaction(ptx)
		tx.Submissions = make([]*ptxapi.PublicTxSubmissionData, len(ptx.Submissions))
		for iSub, pSub := range ptx.Submissions {
			tx.Submissions[iSub] = mapPersistedSubmissionData(pSub)
		}
		results[iTx] = tx
	}
	return results, nil
}

func (ble *pubTxManager) CheckTransactionCompleted(ctx context.Context, id *ptxapi.PublicTxID) (bool, error) {
	// Runs a DB query to see if the transaction is marked completed (for good or bad)
	// A non existent transaction results in false
	var ptxs []*persistedPubTx
	err := ble.p.DB().
		WithContext(ctx).
		Where("transaction = ?", id.Transaction).
		Where("resubmit_idx = ?", id.ResubmitIndex).
		Joins("Completed").
		Select("Completed__tx_hash").
		Limit(1).
		Find(&ptxs).
		Error
	if err != nil {
		return false, err
	}
	if len(ptxs) > 0 && ptxs[0].Completed != nil {
		log.L(ctx).Debugf("CheckTransactionCompleted returned true for %s", ptxs[0].getIDString())
		return true, nil
	}
	return false, nil
}

// the return does NOT include submissions (only the top level TX data)
func (ble *pubTxManager) GetPendingFuelingTransaction(ctx context.Context, sourceAddress tktypes.EthAddress, destinationAddress tktypes.EthAddress) (*ptxapi.PublicTxWithID, error) {
	var ptxs []*persistedPubTx
	err := ble.p.DB().
		WithContext(ctx).
		Where("from = ?", sourceAddress).
		Where("to = ?", destinationAddress).
		Joins("Completed").
		Where("Completed__tx_hash IS NULL").
		Where("data IS NULL").
		Limit(1).
		Find(&ptxs).
		Error
	if err != nil {
		return nil, err
	}
	if len(ptxs) > 0 {
		log.L(ctx).Debugf("GetPendingFuelingTransaction returned %s", ptxs[0].getIDString())
		return mapPersistedTransaction(ptxs[0]), nil
	}
	return nil, nil
}

func (ble *pubTxManager) runTransactionQuery(ctx context.Context, dbTX *gorm.DB, q *gorm.DB) ([]*persistedPubTx, error) {
	var ptxs []*persistedPubTx
	err := q.Find(&ptxs).Error
	if err != nil {
		return nil, err
	}
	signerNonceRefs := make([]string, len(ptxs))
	for i, ptx := range ptxs {
		signerNonceRefs[i] = ptx.SignerNonce
	}
	if len(signerNonceRefs) > 0 {
		allSubs, err := ble.getTransactionSubmissions(ctx, dbTX, signerNonceRefs)
		if err != nil {
			return nil, err
		}
		for _, sub := range allSubs {
			for _, ptx := range ptxs {
				if sub.SignerNonce == ptx.SignerNonce {
					ptx.Submissions = append(ptx.Submissions, sub)
				}
			}
		}
	}
	return ptxs, nil
}

func mapPersistedTransaction(ptx *persistedPubTx) *ptxapi.PublicTxWithID {
	return &ptxapi.PublicTxWithID{
		PublicTxID: ptxapi.PublicTxID{
			Transaction:   ptx.Transaction,
			ResubmitIndex: ptx.ResubmitIndex,
		},
		PublicTx: ptxapi.PublicTx{
			From:    ptx.From,
			Nonce:   tktypes.HexUint64(ptx.Nonce),
			Created: ptx.Created,
			To:      ptx.To,
			Data:    ptx.Data,
			PublicTxOptions: ptxapi.PublicTxOptions{
				Gas:                (*tktypes.HexUint64)(&ptx.Gas),
				Value:              ptx.Value,
				PublicTxGasPricing: recoverGasPriceOptions(ptx.FixedGasPricing),
			},
		},
	}
}

func mapPersistedSubmissionData(pSub *persistedTxSubmission) *ptxapi.PublicTxSubmissionData {
	return &ptxapi.PublicTxSubmissionData{
		Time:               pSub.Created,
		TransactionHash:    tktypes.Bytes32(pSub.TransactionHash),
		PublicTxGasPricing: recoverGasPriceOptions(pSub.GasPricing),
	}
}

func (ble *pubTxManager) getTransactionSubmissions(ctx context.Context, dbTX *gorm.DB, signerNonceRefs []string) ([]*persistedTxSubmission, error) {
	var ptxs []*persistedTxSubmission
	err := dbTX.
		WithContext(ctx).
		Table("public_submissions").
		Where("signer_nonce_ref IN (?)", signerNonceRefs).
		Order("created DESC").
		Error
	return ptxs, err
}

func (ble *pubTxManager) SuspendTransaction(ctx context.Context, from tktypes.EthAddress, nonce uint64) error {
	if err := ble.dispatchAction(ctx, from, nonce, ActionSuspend); err != nil {
		return err
	}
	return nil
}

func (ble *pubTxManager) ResumeTransaction(ctx context.Context, from tktypes.EthAddress, nonce uint64) error {
	if err := ble.dispatchAction(ctx, from, nonce, ActionResume); err != nil {
		return err
	}
	return nil
}

func (pte *pubTxManager) notifyConfirmedTxNonce(addr tktypes.EthAddress, nonce uint64) {
	pte.InFlightOrchestratorMux.Lock()
	defer pte.InFlightOrchestratorMux.Unlock()
	orchestrator := pte.InFlightOrchestrators[addr]
	if orchestrator != nil {
		orchestrator.notifyConfirmedNonceOrchestrator(nonce)
	}
}

func (pte *pubTxManager) UpdateSubStatus(ctx context.Context, imtx InMemoryTxStateReadOnly, subStatus BaseTxSubStatus, action BaseTxAction, info *fftypes.JSONAny, err *fftypes.JSONAny, actionOccurred *tktypes.Timestamp) error {
	// TODO: Choose after testing the right way to treat these records - if text is right or not
	if err == nil {
		pte.rootTxMgr.AddActivityRecord(imtx.GetParentTransactionID(),
			i18n.ExpandWithCode(ctx,
				i18n.MessageKey(msgs.MsgPublicTxHistoryInfo),
				imtx.GetFrom(),
				imtx.GetNonce(),
				subStatus,
				action,
				info.String(),
			),
		)
	} else {
		pte.rootTxMgr.AddActivityRecord(imtx.GetParentTransactionID(),
			i18n.ExpandWithCode(ctx,
				i18n.MessageKey(msgs.MsgPublicTxHistoryError),
				imtx.GetFrom(),
				imtx.GetNonce(),
				subStatus,
				action,
				err,
			),
		)
	}

	return nil
}

func (pte *pubTxManager) MatchUpdateConfirmedTransactions(ctx context.Context, dbTX *gorm.DB, itxs []*blockindexer.IndexedTransaction) ([]*components.PublicTxMatch, error) {

	// Do a DB query in the TX to reverse lookup the TX details we need to match/update the completed status
	// and return the list that matched (which is very possibly none as we only track transactions submitted
	// via our node to the network).
	txiByHash := make(map[tktypes.Bytes32]*blockindexer.IndexedTransaction)
	txHashes := make([]tktypes.Bytes32, len(itxs))
	for i, itx := range itxs {
		txHashes[i] = itx.Hash
		txiByHash[itx.Hash] = itx
	}
	var lookups []*submissionTxReverseLookup
	err := dbTX.
		Table("public_submissions").
		Where("tx_hash IN (?)", txHashes).
		Joins("PublicTx").
		Where("PublicTx__transaction IS NOT NULL").
		Find(&lookups).
		Error
	if err != nil {
		return nil, err
	}

	// Correlate our results with the inputs to build:
	// - the result to return so the Paladin tx manager knows the linkage
	// - the insert we will do in this DB transaction of all the completion records
	results := make([]*components.PublicTxMatch, len(lookups))
	completions := make([]*publicCompletion, len(lookups))
	for i, match := range lookups {
		txi := txiByHash[match.TransactionHash]
		results[i] = &components.PublicTxMatch{
			PublicTxID: ptxapi.PublicTxID{
				Transaction:   match.PublicTx.Transaction,
				ResubmitIndex: match.PublicTx.ResubmitIndex,
			},
			IndexedTransaction: txi,
		}
		completions[i] = &publicCompletion{
			SignerNonce:     match.SignerNonce,
			TransactionHash: match.TransactionHash,
		}
	}

	if len(completions) > 0 {
		// We have some contracts to persist
		err := dbTX.
			Table("public_completions").
			Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "signer_nonce"}},
				DoNothing: true, // immutable
			}).
			Create(completions).
			Error
		if err != nil {
			return nil, err
		}
	}

	return results, nil

}

// We've got to be super careful not to block this thread, so we treat this just like a suspend/resume
// on each of these transactions
func (pte *pubTxManager) NotifyConfirmPersisted(ctx context.Context, confirms []*components.PublicTxMatch) {
	// for _, conf := range confirms {
	// 	pte.dispatchAction(ctx)
	// }
}
