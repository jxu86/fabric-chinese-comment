/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ledger

import (
	"fmt"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric-lib-go/healthz"
	commonledger "github.com/hyperledger/fabric/common/ledger"
	"github.com/hyperledger/fabric/common/metrics"
	"github.com/hyperledger/fabric/protos/common"
	"github.com/hyperledger/fabric/protos/ledger/rwset"
	"github.com/hyperledger/fabric/protos/ledger/rwset/kvrwset"
	"github.com/hyperledger/fabric/protos/peer"
)

// Initializer encapsulates dependencies for PeerLedgerProvider
type Initializer struct {
	StateListeners                []StateListener
	DeployedChaincodeInfoProvider DeployedChaincodeInfoProvider
	MembershipInfoProvider        MembershipInfoProvider
	MetricsProvider               metrics.Provider
	HealthCheckRegistry           HealthCheckRegistry
}

// PeerLedgerProvider provides handle to ledger instances
type PeerLedgerProvider interface {
	Initialize(initializer *Initializer) error
	// Create creates a new ledger with the given genesis block.
	// This function guarantees that the creation of ledger and committing the genesis block would an atomic action
	// The chain id retrieved from the genesis block is treated as a ledger id
	Create(genesisBlock *common.Block) (PeerLedger, error) // 用给定的创世纪块创建Ledger
	// Open opens an already created ledger
	Open(ledgerID string) (PeerLedger, error) // 打开已创建的Ledger
	// Exists tells whether the ledger with given id exists
	Exists(ledgerID string) (bool, error)		// 按ledgerID查Ledger是否存在
	// List lists the ids of the existing ledgers
	List() ([]string, error)					// 列出现有的ledgerID
	// Close closes the PeerLedgerProvider
	Close()										// 关闭 PeerLedgerProvider
}

// PeerLedger differs from the OrdererLedger in that PeerLedger locally maintain a bitmask
// that tells apart valid transactions from invalid ones
type PeerLedger interface {
	commonledger.Ledger
	// GetTransactionByID retrieves a transaction by id
	GetTransactionByID(txID string) (*peer.ProcessedTransaction, error)
	// GetBlockByHash returns a block given it's hash
	GetBlockByHash(blockHash []byte) (*common.Block, error)
	// GetBlockByTxID returns a block which contains a transaction
	GetBlockByTxID(txID string) (*common.Block, error)
	// GetTxValidationCodeByTxID returns reason code of transaction validation
	GetTxValidationCodeByTxID(txID string) (peer.TxValidationCode, error)
	// NewTxSimulator gives handle to a transaction simulator.
	// A client can obtain more than one 'TxSimulator's for parallel execution.
	// Any snapshoting/synchronization should be performed at the implementation level if required
	NewTxSimulator(txid string) (TxSimulator, error)
	// NewQueryExecutor gives handle to a query executor.
	// A client can obtain more than one 'QueryExecutor's for parallel execution.
	// Any synchronization should be performed at the implementation level if required
	NewQueryExecutor() (QueryExecutor, error)
	// NewHistoryQueryExecutor gives handle to a history query executor.
	// A client can obtain more than one 'HistoryQueryExecutor's for parallel execution.
	// Any synchronization should be performed at the implementation level if required
	NewHistoryQueryExecutor() (HistoryQueryExecutor, error)
	// GetPvtDataAndBlockByNum returns the block and the corresponding pvt data.
	// The pvt data is filtered by the list of 'ns/collections' supplied
	// A nil filter does not filter any results and causes retrieving all the pvt data for the given blockNum
	GetPvtDataAndBlockByNum(blockNum uint64, filter PvtNsCollFilter) (*BlockAndPvtData, error)
	// GetPvtDataByNum returns only the pvt data  corresponding to the given block number
	// The pvt data is filtered by the list of 'ns/collections' supplied in the filter
	// A nil filter does not filter any results and causes retrieving all the pvt data for the given blockNum
	GetPvtDataByNum(blockNum uint64, filter PvtNsCollFilter) ([]*TxPvtData, error)
	// CommitWithPvtData commits the block and the corresponding pvt data in an atomic operation
	CommitWithPvtData(blockAndPvtdata *BlockAndPvtData, commitOpts *CommitOptions) error
	// Purge removes private read-writes set generated by endorsers at block height lesser than
	// a given maxBlockNumToRetain. In other words, Purge only retains private read-write sets
	// that were generated at block height of maxBlockNumToRetain or higher.
	PurgePrivateData(maxBlockNumToRetain uint64) error
	// PrivateDataMinBlockNum returns the lowest retained endorsement block height
	PrivateDataMinBlockNum() (uint64, error)
	//Prune prunes the blocks/transactions that satisfy the given policy
	Prune(policy commonledger.PrunePolicy) error
	// GetConfigHistoryRetriever returns the ConfigHistoryRetriever
	GetConfigHistoryRetriever() (ConfigHistoryRetriever, error)
	// CommitPvtDataOfOldBlocks commits the private data corresponding to already committed block
	// If hashes for some of the private data supplied in this function does not match
	// the corresponding hash present in the block, the unmatched private data is not
	// committed and instead the mismatch inforation is returned back
	CommitPvtDataOfOldBlocks(blockPvtData []*BlockPvtData) ([]*PvtdataHashMismatch, error)
	// GetMissingPvtDataTracker return the MissingPvtDataTracker
	GetMissingPvtDataTracker() (MissingPvtDataTracker, error)
	// DoesPvtDataInfoExist returns true when
	// (1) the ledger has pvtdata associated with the given block number (or)
	// (2) a few or all pvtdata associated with the given block number is missing but the
	//     missing info is recorded in the ledger (or)
	// (3) the block is committed and does not contain any pvtData.
	DoesPvtDataInfoExist(blockNum uint64) (bool, error)
}

// ValidatedLedger represents the 'final ledger' after filtering out invalid transactions from PeerLedger.
// Post-v1
type ValidatedLedger interface {
	commonledger.Ledger
}

// SimpleQueryExecutor encapsulates basic functions
type SimpleQueryExecutor interface {
	// GetState gets the value for given namespace and key. For a chaincode, the namespace corresponds to the chaincodeId
	GetState(namespace string, key string) ([]byte, error)
	// GetStateRangeScanIterator returns an iterator that contains all the key-values between given key ranges.
	// startKey is included in the results and endKey is excluded. An empty startKey refers to the first available key
	// and an empty endKey refers to the last available key. For scanning all the keys, both the startKey and the endKey
	// can be supplied as empty strings. However, a full scan should be used judiciously for performance reasons.
	// The returned ResultsIterator contains results of type *KV which is defined in protos/ledger/queryresult.
	GetStateRangeScanIterator(namespace string, startKey string, endKey string) (commonledger.ResultsIterator, error)
}

// QueryExecutor executes the queries
// Get* methods are for supporting KV-based data model. ExecuteQuery method is for supporting a rich datamodel and query support
//
// ExecuteQuery method in the case of a rich data model is expected to support queries on
// latest state, historical state and on the intersection of state and transactions
type QueryExecutor interface {
	SimpleQueryExecutor
	// GetStateMetadata returns the metadata for given namespace and key
	GetStateMetadata(namespace, key string) (map[string][]byte, error)
	// GetStateMultipleKeys gets the values for multiple keys in a single call
	GetStateMultipleKeys(namespace string, keys []string) ([][]byte, error)
	// GetStateRangeScanIteratorWithMetadata returns an iterator that contains all the key-values between given key ranges.
	// startKey is included in the results and endKey is excluded. An empty startKey refers to the first available key
	// and an empty endKey refers to the last available key. For scanning all the keys, both the startKey and the endKey
	// can be supplied as empty strings. However, a full scan should be used judiciously for performance reasons.
	// metadata is a map of additional query parameters
	// The returned ResultsIterator contains results of type *KV which is defined in protos/ledger/queryresult.
	GetStateRangeScanIteratorWithMetadata(namespace string, startKey, endKey string, metadata map[string]interface{}) (QueryResultsIterator, error)
	// ExecuteQuery executes the given query and returns an iterator that contains results of type specific to the underlying data store.
	// Only used for state databases that support query
	// For a chaincode, the namespace corresponds to the chaincodeId
	// The returned ResultsIterator contains results of type *KV which is defined in protos/ledger/queryresult.
	ExecuteQuery(namespace, query string) (commonledger.ResultsIterator, error)
	// ExecuteQueryWithMetadata executes the given query and returns an iterator that contains results of type specific to the underlying data store.
	// metadata is a map of additional query parameters
	// Only used for state databases that support query
	// For a chaincode, the namespace corresponds to the chaincodeId
	// The returned ResultsIterator contains results of type *KV which is defined in protos/ledger/queryresult.
	ExecuteQueryWithMetadata(namespace, query string, metadata map[string]interface{}) (QueryResultsIterator, error)
	// GetPrivateData gets the value of a private data item identified by a tuple <namespace, collection, key>
	GetPrivateData(namespace, collection, key string) ([]byte, error)
	// GetPrivateDataHash gets the hash of the value of a private data item identified by a tuple <namespace, collection, key>
	// Function `GetPrivateData` is only meaningful when it is invoked on a peer that is authorized to have the private data
	// for the collection <namespace, collection>. However, the function `GetPrivateDataHash` can be invoked on any peer
	// to get the hash of the current value
	GetPrivateDataHash(namespace, collection, key string) ([]byte, error)
	// GetPrivateDataMetadata gets the metadata of a private data item identified by a tuple <namespace, collection, key>
	GetPrivateDataMetadata(namespace, collection, key string) (map[string][]byte, error)
	// GetPrivateDataMetadataByHash gets the metadata of a private data item identified by a tuple <namespace, collection, keyhash>
	GetPrivateDataMetadataByHash(namespace, collection string, keyhash []byte) (map[string][]byte, error)
	// GetPrivateDataMultipleKeys gets the values for the multiple private data items in a single call
	GetPrivateDataMultipleKeys(namespace, collection string, keys []string) ([][]byte, error)
	// GetPrivateDataRangeScanIterator returns an iterator that contains all the key-values between given key ranges.
	// startKey is included in the results and endKey is excluded. An empty startKey refers to the first available key
	// and an empty endKey refers to the last available key. For scanning all the keys, both the startKey and the endKey
	// can be supplied as empty strings. However, a full scan shuold be used judiciously for performance reasons.
	// The returned ResultsIterator contains results of type *KV which is defined in protos/ledger/queryresult.
	GetPrivateDataRangeScanIterator(namespace, collection, startKey, endKey string) (commonledger.ResultsIterator, error)
	// ExecuteQuery executes the given query and returns an iterator that contains results of type specific to the underlying data store.
	// Only used for state databases that support query
	// For a chaincode, the namespace corresponds to the chaincodeId
	// The returned ResultsIterator contains results of type *KV which is defined in protos/ledger/queryresult.
	ExecuteQueryOnPrivateData(namespace, collection, query string) (commonledger.ResultsIterator, error)
	// Done releases resources occupied by the QueryExecutor
	Done()
}

// HistoryQueryExecutor executes the history queries
type HistoryQueryExecutor interface {
	// GetHistoryForKey retrieves the history of values for a key.
	// The returned ResultsIterator contains results of type *KeyModification which is defined in protos/ledger/queryresult.
	GetHistoryForKey(namespace string, key string) (commonledger.ResultsIterator, error)
}

// TxSimulator simulates a transaction on a consistent snapshot of the 'as recent state as possible'
// Set* methods are for supporting KV-based data model. ExecuteUpdate method is for supporting a rich datamodel and query support
type TxSimulator interface {
	QueryExecutor
	// SetState sets the given value for the given namespace and key. For a chaincode, the namespace corresponds to the chaincodeId
	SetState(namespace string, key string, value []byte) error
	// DeleteState deletes the given namespace and key
	DeleteState(namespace string, key string) error
	// SetMultipleKeys sets the values for multiple keys in a single call
	SetStateMultipleKeys(namespace string, kvs map[string][]byte) error
	// SetStateMetadata sets the metadata associated with an existing key-tuple <namespace, key>
	SetStateMetadata(namespace, key string, metadata map[string][]byte) error
	// DeleteStateMetadata deletes the metadata (if any) associated with an existing key-tuple <namespace, key>
	DeleteStateMetadata(namespace, key string) error
	// ExecuteUpdate for supporting rich data model (see comments on QueryExecutor above)
	ExecuteUpdate(query string) error
	// SetPrivateData sets the given value to a key in the private data state represented by the tuple <namespace, collection, key>
	SetPrivateData(namespace, collection, key string, value []byte) error
	// SetPrivateDataMultipleKeys sets the values for multiple keys in the private data space in a single call
	SetPrivateDataMultipleKeys(namespace, collection string, kvs map[string][]byte) error
	// DeletePrivateData deletes the given tuple <namespace, collection, key> from private data
	DeletePrivateData(namespace, collection, key string) error
	// SetPrivateDataMetadata sets the metadata associated with an existing key-tuple <namespace, collection, key>
	SetPrivateDataMetadata(namespace, collection, key string, metadata map[string][]byte) error
	// DeletePrivateDataMetadata deletes the metadata associated with an existing key-tuple <namespace, collection, key>
	DeletePrivateDataMetadata(namespace, collection, key string) error
	// GetTxSimulationResults encapsulates the results of the transaction simulation.
	// This should contain enough detail for
	// - The update in the state that would be caused if the transaction is to be committed
	// - The environment in which the transaction is executed so as to be able to decide the validity of the environment
	//   (at a later time on a different peer) during committing the transactions
	// Different ledger implementation (or configurations of a single implementation) may want to represent the above two pieces
	// of information in different way in order to support different data-models or optimize the information representations.
	// Returned type 'TxSimulationResults' contains the simulation results for both the public data and the private data.
	// The public data simulation results are expected to be used as in V1 while the private data simulation results are expected
	// to be used by the gossip to disseminate this to the other endorsers (in phase-2 of sidedb)
	GetTxSimulationResults() (*TxSimulationResults, error)
}

// QueryResultsIterator - an iterator for query result set
type QueryResultsIterator interface {
	commonledger.ResultsIterator
	// GetBookmarkAndClose returns a paging bookmark and releases resources occupied by the iterator
	GetBookmarkAndClose() string
}

// TxPvtData encapsulates the transaction number and pvt write-set for a transaction
type TxPvtData struct {
	SeqInBlock uint64
	WriteSet   *rwset.TxPvtReadWriteSet
}

// TxPvtDataMap is a map from txNum to the pvtData
type TxPvtDataMap map[uint64]*TxPvtData

// MissingPvtData contains a namespace and collection for
// which the pvtData is not present. It also denotes
// whether the missing pvtData is eligible (i.e., whether
// the peer is member of the [namespace, collection]
type MissingPvtData struct {
	Namespace  string
	Collection string
	IsEligible bool
}

// TxMissingPvtDataMap is a map from txNum to the list of
// missing pvtData
type TxMissingPvtDataMap map[uint64][]*MissingPvtData

// BlockAndPvtData encapsulates the block and a map that contains the tuples <seqInBlock, *TxPvtData>
// The map is expected to contain the entries only for the transactions that has associated pvt data
type BlockAndPvtData struct {
	Block          *common.Block
	PvtData        TxPvtDataMap
	MissingPvtData TxMissingPvtDataMap
}

// BlockPvtData contains the private data for a block
type BlockPvtData struct {
	BlockNum  uint64
	WriteSets TxPvtDataMap
}

// Add adds a given missing private data in the MissingPrivateDataList
func (txMissingPvtData TxMissingPvtDataMap) Add(txNum uint64, ns, coll string, isEligible bool) {
	txMissingPvtData[txNum] = append(txMissingPvtData[txNum], &MissingPvtData{ns, coll, isEligible})
}

// CommitOptions encapsulates options associated with a block commit.
type CommitOptions struct {
	FetchPvtDataFromLedger bool
}

// PvtCollFilter represents the set of the collection names (as keys of the map with value 'true')
type PvtCollFilter map[string]bool

// PvtNsCollFilter specifies the tuple <namespace, PvtCollFilter>
type PvtNsCollFilter map[string]PvtCollFilter

// NewPvtNsCollFilter constructs an empty PvtNsCollFilter
func NewPvtNsCollFilter() PvtNsCollFilter {
	return make(map[string]PvtCollFilter)
}

// Has returns true if the pvtdata includes the data for collection <ns,coll>
func (pvtdata *TxPvtData) Has(ns string, coll string) bool {
	if pvtdata.WriteSet == nil {
		return false
	}
	for _, nsdata := range pvtdata.WriteSet.NsPvtRwset {
		if nsdata.Namespace == ns {
			for _, colldata := range nsdata.CollectionPvtRwset {
				if colldata.CollectionName == coll {
					return true
				}
			}
		}
	}
	return false
}

// Add adds a namespace-collection tuple to the filter
func (filter PvtNsCollFilter) Add(ns string, coll string) {
	collFilter, ok := filter[ns]
	if !ok {
		collFilter = make(map[string]bool)
		filter[ns] = collFilter
	}
	collFilter[coll] = true
}

// Has returns true if the filter has the entry for tuple namespace-collection
func (filter PvtNsCollFilter) Has(ns string, coll string) bool {
	collFilter, ok := filter[ns]
	if !ok {
		return false
	}
	return collFilter[coll]
}

// TxSimulationResults captures the details of the simulation results
type TxSimulationResults struct {
	PubSimulationResults *rwset.TxReadWriteSet
	PvtSimulationResults *rwset.TxPvtReadWriteSet
}

// GetPubSimulationBytes returns the serialized bytes of public readwrite set
func (txSim *TxSimulationResults) GetPubSimulationBytes() ([]byte, error) {
	return proto.Marshal(txSim.PubSimulationResults)
}

// GetPvtSimulationBytes returns the serialized bytes of private readwrite set
func (txSim *TxSimulationResults) GetPvtSimulationBytes() ([]byte, error) {
	if !txSim.ContainsPvtWrites() {
		return nil, nil
	}
	return proto.Marshal(txSim.PvtSimulationResults)
}

// ContainsPvtWrites returns true if the simulation results include the private writes
func (txSim *TxSimulationResults) ContainsPvtWrites() bool {
	return txSim.PvtSimulationResults != nil
}

//go:generate counterfeiter -o mock/state_listener.go -fake-name StateListener . StateListener

// StateListener allows a custom code for performing additional stuff upon state change
// for a perticular namespace against which the listener is registered.
// This helps to perform custom tasks other than the state updates.
// A ledger implemetation is expected to invoke Function `HandleStateUpdates` once per block and
// the `stateUpdates` parameter passed to the function captures the state changes caused by the block
// for the namespace. The actual data type of stateUpdates depends on the data model enabled.
// For instance, for KV data model, the actual type would be proto message
// `github.com/hyperledger/fabric/protos/ledger/rwset/kvrwset.KVWrite`
// Function `HandleStateUpdates` is expected to be invoked before block is committed and if this
// function returns an error, the ledger implementation is expected to halt block commit operation
// and result in a panic
type StateListener interface {
	InterestedInNamespaces() []string
	HandleStateUpdates(trigger *StateUpdateTrigger) error
	StateCommitDone(channelID string)
}

// StateUpdateTrigger encapsulates the information and helper tools that may be used by a StateListener
type StateUpdateTrigger struct {
	LedgerID                    string
	StateUpdates                StateUpdates
	CommittingBlockNum          uint64
	CommittedStateQueryExecutor SimpleQueryExecutor
	PostCommitQueryExecutor     SimpleQueryExecutor
}

// StateUpdates is the generic type to represent the state updates
type StateUpdates map[string]interface{}

// ConfigHistoryRetriever allow retrieving history of collection configs
type ConfigHistoryRetriever interface {
	CollectionConfigAt(blockNum uint64, chaincodeName string) (*CollectionConfigInfo, error)
	MostRecentCollectionConfigBelow(blockNum uint64, chaincodeName string) (*CollectionConfigInfo, error)
}

// MissingPvtDataTracker allows getting information about the private data that is not missing on the peer
type MissingPvtDataTracker interface {
	GetMissingPvtDataInfoForMostRecentBlocks(maxBlocks int) (MissingPvtDataInfo, error)
}

// MissingPvtDataInfo is a map of block number to MissingBlockPvtdataInfo
type MissingPvtDataInfo map[uint64]MissingBlockPvtdataInfo

// MissingBlockPvtdataInfo is a map of transaction number (within the block) to MissingCollectionPvtDataInfo
type MissingBlockPvtdataInfo map[uint64][]*MissingCollectionPvtDataInfo

// MissingCollectionPvtDataInfo includes the name of the chaincode and collection for which private data is missing
type MissingCollectionPvtDataInfo struct {
	Namespace, Collection string
}

// CollectionConfigInfo encapsulates a collection config for a chaincode and its committing block number
type CollectionConfigInfo struct {
	CollectionConfig   *common.CollectionConfigPackage
	CommittingBlockNum uint64
}

// Add adds a missing data entry to the MissingPvtDataInfo Map
func (missingPvtDataInfo MissingPvtDataInfo) Add(blkNum, txNum uint64, ns, coll string) {
	missingBlockPvtDataInfo, ok := missingPvtDataInfo[blkNum]
	if !ok {
		missingBlockPvtDataInfo = make(MissingBlockPvtdataInfo)
		missingPvtDataInfo[blkNum] = missingBlockPvtDataInfo
	}

	if _, ok := missingBlockPvtDataInfo[txNum]; !ok {
		missingBlockPvtDataInfo[txNum] = []*MissingCollectionPvtDataInfo{}
	}

	missingBlockPvtDataInfo[txNum] = append(missingBlockPvtDataInfo[txNum],
		&MissingCollectionPvtDataInfo{
			Namespace:  ns,
			Collection: coll})
}

// ErrCollectionConfigNotYetAvailable is an error which is returned from the function
// ConfigHistoryRetriever.CollectionConfigAt() if the latest block number committed
// is lower than the block number specified in the request.
type ErrCollectionConfigNotYetAvailable struct {
	MaxBlockNumCommitted uint64
	Msg                  string
}

func (e *ErrCollectionConfigNotYetAvailable) Error() string {
	return e.Msg
}

// NotFoundInIndexErr is used to indicate missing entry in the index
type NotFoundInIndexErr string

func (NotFoundInIndexErr) Error() string {
	return "Entry not found in index"
}

// CollConfigNotDefinedError is returned whenever an operation
// is requested on a collection whose config has not been defined
type CollConfigNotDefinedError struct {
	Ns string
}

func (e *CollConfigNotDefinedError) Error() string {
	return fmt.Sprintf("collection config not defined for chaincode [%s], pass the collection configuration upon chaincode definition/instantiation", e.Ns)
}

// InvalidCollNameError is returned whenever an operation
// is requested on a collection whose name is invalid
type InvalidCollNameError struct {
	Ns, Coll string
}

func (e *InvalidCollNameError) Error() string {
	return fmt.Sprintf("collection [%s] not defined in the collection config for chaincode [%s]", e.Coll, e.Ns)
}

// PvtdataHashMismatch is used when the hash of private write-set
// does not match the corresponding hash present in the block
// See function `PeerLedger.CommitPvtData` for the usages
type PvtdataHashMismatch struct {
	BlockNum, TxNum       uint64
	Namespace, Collection string
	ExpectedHash          []byte
}

// DeployedChaincodeInfoProvider is a dependency that is used by ledger to build collection config history
// LSCC module is expected to provide an implementation fo this dependencys
type DeployedChaincodeInfoProvider interface {
	Namespaces() []string
	UpdatedChaincodes(stateUpdates map[string][]*kvrwset.KVWrite) ([]*ChaincodeLifecycleInfo, error)
	ChaincodeInfo(chaincodeName string, qe SimpleQueryExecutor) (*DeployedChaincodeInfo, error)
	CollectionInfo(chaincodeName, collectionName string, qe SimpleQueryExecutor) (*common.StaticCollectionConfig, error)
}

// DeployedChaincodeInfo encapsulates chaincode information from the deployed chaincodes
type DeployedChaincodeInfo struct {
	Name                string
	Hash                []byte
	Version             string
	CollectionConfigPkg *common.CollectionConfigPackage
}

// ChaincodeLifecycleInfo captures the update info of a chaincode
type ChaincodeLifecycleInfo struct {
	Name    string
	Deleted bool
	Details *ChaincodeLifecycleDetails // Can contain finer details about lifecycle event that can be used for certain optimization
}

// ChaincodeLifecycleDetails captures the finer details of chaincode lifecycle event
type ChaincodeLifecycleDetails struct {
	Updated bool // true, if an existing chaincode is updated (false for newly deployed chaincodes).
	// Following attributes are meaningful only if 'Updated' is true
	HashChanged        bool     // true, if the chaincode code package is changed
	CollectionsUpdated []string // names of the collections that are either added or updated
	CollectionsRemoved []string // names of the collections that are removed
}

// MembershipInfoProvider is a dependency that is used by ledger to determine whether the current peer is
// a member of a collection. Gossip module is expected to provide the dependency to ledger
type MembershipInfoProvider interface {
	// AmMemberOf checks whether the current peer is a member of the given collection
	AmMemberOf(channelName string, collectionPolicyConfig *common.CollectionPolicyConfig) (bool, error)
}

//go:generate counterfeiter -o mock/deployed_ccinfo_provider.go -fake-name DeployedChaincodeInfoProvider . DeployedChaincodeInfoProvider
//go:generate counterfeiter -o mock/membership_info_provider.go -fake-name MembershipInfoProvider . MembershipInfoProvider

//go:generate counterfeiter -o mock/health_check_registry.go -fake-name HealthCheckRegistry . HealthCheckRegistry

type HealthCheckRegistry interface {
	RegisterChecker(string, healthz.HealthChecker) error
}
