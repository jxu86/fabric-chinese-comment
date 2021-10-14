/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package solo

import (
	"fmt"
	"time"

	cb "github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/orderer/consensus"
	"github.com/pkg/errors"
)

var logger = flogging.MustGetLogger("orderer.consensus.solo")

type consenter struct{}

type chain struct {
	support  consensus.ConsenterSupport
	sendChan chan *message
	exitChan chan struct{}
}

type message struct {
	configSeq uint64
	normalMsg *cb.Envelope
	configMsg *cb.Envelope
}

// New creates a new consenter for the solo consensus scheme.
// The solo consensus scheme is very simple, and allows only one consenter for a given chain (this process).
// It accepts messages being delivered via Order/Configure, orders them, and then uses the blockcutter to form the messages
// into blocks before writing to the given ledger
func New() consensus.Consenter {
	return &consenter{}
}

func (solo *consenter) HandleChain(support consensus.ConsenterSupport, metadata *cb.Metadata) (consensus.Chain, error) {
	logger.Warningf("Use of the Solo orderer is deprecated and remains only for use in test environments but may be removed in the future.")
	return newChain(support), nil
}

func (c *consenter) JoinChain(support consensus.ConsenterSupport, joinBlock *cb.Block) (consensus.Chain, error) {
	return nil, errors.New("the Solo orderer does not support JoinChain")
}

func newChain(support consensus.ConsenterSupport) *chain {
	return &chain{
		support:  support,
		sendChan: make(chan *message),
		exitChan: make(chan struct{}),
	}
}

func (ch *chain) Start() {
	go ch.main()
}

func (ch *chain) Halt() {
	select {
	case <-ch.exitChan:
		// Allow multiple halts without panic
	default:
		close(ch.exitChan)
	}
}

func (ch *chain) WaitReady() error {
	return nil
}

// Order accepts normal messages for ordering
func (ch *chain) Order(env *cb.Envelope, configSeq uint64) error {
	select {
	case ch.sendChan <- &message{
		configSeq: configSeq,
		normalMsg: env,
	}:
		return nil
	case <-ch.exitChan:
		return fmt.Errorf("Exiting")
	}
}

// Configure accepts configuration update messages for ordering
func (ch *chain) Configure(config *cb.Envelope, configSeq uint64) error {
	select {
	case ch.sendChan <- &message{
		configSeq: configSeq,
		configMsg: config,
	}:
		return nil
	case <-ch.exitChan:
		return fmt.Errorf("Exiting")
	}
}

// Errored only closes on exit
func (ch *chain) Errored() <-chan struct{} {
	return ch.exitChan
}

func (ch *chain) main() {
	// 创建打包超时定时器
	var timer <-chan time.Time
	var err error
	// 循环接收消息
	for {
		// 获取当前的配置序号
		seq := ch.support.Sequence()
		err = nil
		select {
		case msg := <-ch.sendChan:
			if msg.configMsg == nil {
				// NormalMsg
				if msg.configSeq < seq {
					_, err = ch.support.ProcessNormalMsg(msg.normalMsg)
					if err != nil {
						logger.Warningf("Discarding bad normal message: %s", err)
						continue
					}
				}
				// 把交易分组，每组为一个batch
				batches, pending := ch.support.BlockCutter().Ordered(msg.normalMsg)

				for _, batch := range batches {
					block := ch.support.CreateNextBlock(batch)
					ch.support.WriteBlock(block, nil)
				}

				switch {
				case timer != nil && !pending: // 如果计时开始但缓冲区没数据，把计时关掉
					// Timer is already running but there are no messages pending, stop the timer
					timer = nil
				case timer == nil && pending: // 如果计时没开始但缓冲区有数据，开始计时
					// Timer is not already running and there are messages pending, so start it
					timer = time.After(ch.support.SharedConfig().BatchTimeout())
					logger.Debugf("Just began %s batch timer", ch.support.SharedConfig().BatchTimeout().String())
				default:
					// Do nothing when:
					// 1. Timer is already running and there are messages pending
					// 2. Timer is not set and there are no messages pending
				}

			} else {
				// ConfigMsg
				// 消息中的配置序号小于通道最新配置序号，那么该消息就有可能会存在失效的问题
				if msg.configSeq < seq {
					msg.configMsg, _, err = ch.support.ProcessConfigMsg(msg.configMsg)
					if err != nil {
						logger.Warningf("Discarding bad config message: %s", err)
						continue
					}
				}
				// 把交易分组，每组为一个batch
				batch := ch.support.BlockCutter().Cut()
				if batch != nil {
					block := ch.support.CreateNextBlock(batch)
					ch.support.WriteBlock(block, nil)
				}
				// 创建一个新的区块
				block := ch.support.CreateNextBlock([]*cb.Envelope{msg.configMsg})
				// 写区块
				ch.support.WriteConfigBlock(block, nil)
				timer = nil
			}
		// 打包BatchTimeout超时定时器触发
		case <-timer:
			//clear the timer
			timer = nil
			// 直接对缓存的message进行Cut打包
			batch := ch.support.BlockCutter().Cut()
			// 如果batch为空，说明无消息可打包，继续等待消息
			if len(batch) == 0 {
				logger.Warningf("Batch timer expired with no pending requests, this might indicate a bug")
				continue
			}
			// 创建区块，写入账本
			logger.Debugf("Batch timer expired, creating block")
			block := ch.support.CreateNextBlock(batch)
			ch.support.WriteBlock(block, nil)
		// 接收到exit chan时退出主逻辑
		case <-ch.exitChan:
			logger.Debugf("Exiting")
			return
		}
	}
}
