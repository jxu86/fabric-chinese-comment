/*
Copyright IBM Corp. 2016 All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package fileledger

import (
	"sync"

	"github.com/hyperledger/fabric/common/ledger/blkstorage"
	"github.com/hyperledger/fabric/common/ledger/blockledger"
	"github.com/hyperledger/fabric/common/metrics"
)

type blockStoreProvider interface {
	Open(ledgerid string) (*blkstorage.BlockStore, error)
	List() ([]string, error)
	Close()
}

type fileLedgerFactory struct {
	blkstorageProvider blockStoreProvider
	ledgers            map[string]blockledger.ReadWriter // 账本字典，记录通道ID与区块链账本的映射关系，管理所有通道的账本对象(FileLedger)
	mutex              sync.Mutex                        // 同步锁
}

// GetOrCreate gets an existing ledger (if it exists) or creates it if it does not
// 从ledgers中查找，如找到则返回，否则创建Ledger（即blkstorage）并构造fileLedger
func (flf *fileLedgerFactory) GetOrCreate(chainID string) (blockledger.ReadWriter, error) {
	flf.mutex.Lock()
	defer flf.mutex.Unlock()

	key := chainID
	// check cache
	ledger, ok := flf.ledgers[key]
	if ok {
		return ledger, nil
	}
	// open fresh
	blockStore, err := flf.blkstorageProvider.Open(key)
	if err != nil {
		return nil, err
	}
	ledger = NewFileLedger(blockStore)
	flf.ledgers[key] = ledger
	return ledger, nil
}

// ChannelIDs returns the channel IDs the factory is aware of
// 获取已存在的Ledger列表，调取flf.blkstorageProvider.List()
func (flf *fileLedgerFactory) ChannelIDs() []string {
	channelIDs, err := flf.blkstorageProvider.List()
	if err != nil {
		logger.Panic(err)
	}
	return channelIDs
}

// Close releases all resources acquired by the factory
// 关闭并释放资源flf.blkstorageProvider.Close()
func (flf *fileLedgerFactory) Close() {
	flf.blkstorageProvider.Close()
}

// New creates a new ledger factory
// 创建一个新的账本工厂,返回fileLedgerFactory结构体
func New(directory string, metricsProvider metrics.Provider) (blockledger.Factory, error) {
	p, err := blkstorage.NewProvider(
		blkstorage.NewConf(directory, -1),
		&blkstorage.IndexConfig{
			AttrsToIndex: []blkstorage.IndexableAttr{blkstorage.IndexableAttrBlockNum}},
		metricsProvider,
	)
	if err != nil {
		return nil, err
	}
	return &fileLedgerFactory{
		blkstorageProvider: p,
		ledgers:            make(map[string]blockledger.ReadWriter),
	}, nil
}
