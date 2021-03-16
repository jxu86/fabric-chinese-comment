/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package multichannel

import (
	cb "github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric/bccsp"
	"github.com/hyperledger/fabric/common/channelconfig"
	"github.com/hyperledger/fabric/common/ledger/blockledger"
	"github.com/hyperledger/fabric/common/policies"
	"github.com/hyperledger/fabric/internal/pkg/identity"
	"github.com/hyperledger/fabric/orderer/common/blockcutter"
	"github.com/hyperledger/fabric/orderer/common/msgprocessor"
	"github.com/hyperledger/fabric/orderer/common/types"
	"github.com/hyperledger/fabric/orderer/consensus"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/pkg/errors"
)

// ChainSupport holds the resources for a particular channel.
type ChainSupport struct {
	// 账本资源对象封装了通道配置资源对象（configResources类 型）与区块账本对象（FileLedger类型）
	*ledgerResources
	// 负责过滤处理应用通道上的消息，以筛选出符合 通道要求的消息，
	// 默认初始化4个标准通道消息过滤器，即Empty-RejectRule拒绝空消息过滤器、 expirationRejectRule拒绝过期的签名者身份证书的过滤器、
	// MaxBytesRule验证消息最大字节数（默认 98MB）的过滤器和sigFilter验证消息签名是否满足ChannelWriters（/Channel/Writers）通道写权限策略 要求的过滤器。
	msgprocessor.Processor
	// 负责构造新区块并向账本提交区块文件，同时创建新的应 用通道与更新通道配置。
	// 该对象在初始化时设置最新的区块号lastBlock、通道配置序号lastConfigSeq、 最新的配置区块号lastConfigBlockNum、
	// 多通道注册管理器Registrar对象（用于创建新的应用通道）以 及关联通道的链支持对象（用于更新通道配置）。
	*BlockWriter
	// 采用共识排序后端对交易排序，再添加到缓存交易消 息列表，
	// 同时利用链支持对象上的消息切割组件、通道消息处理器、区块账本写组件等模块执行打包 出块、通道管理等操作。
	consensus.Chain
	// 消息切割组件
	// 获取指定通道上 的Orderer配置，包含共识组件类型、交易出块周期时间、区块最大字节数、通道限制参数（如通道数 量）等。
	// 接着，基于该配置创建消息切割组件（receiver类型），将本地的缓存交易消息列表按照交易 出块规则切割成批量交易集合（[]*cb.Envelope类型），
	// 再交由区块账本写组件构造新区块，并提交到 账本区块文件
	cutter blockcutter.Receiver
	identity.SignerSerializer
	BCCSP bccsp.BCCSP

	// NOTE: It makes sense to add this to the ChainSupport since the design of Registrar does not assume
	// that there is a single consensus type at this orderer node and therefore the resolution of
	// the consensus type too happens only at the ChainSupport level.
	consensus.MetadataValidator

	// The registrar is not aware of the exact type that the Chain is, e.g. etcdraft, inactive, or follower.
	// Therefore, we let each chain report its cluster relation and status through this interface. Non cluster
	// type chains (solo, kafka) are assigned a static reporter.
	consensus.StatusReporter
}

func newChainSupport(
	registrar *Registrar,
	ledgerResources *ledgerResources,
	consenters map[string]consensus.Consenter,
	signer identity.SignerSerializer,
	blockcutterMetrics *blockcutter.Metrics,
	bccsp bccsp.BCCSP,
) (*ChainSupport, error) {
	// Read in the last block and metadata for the channel
	lastBlock := blockledger.GetBlock(ledgerResources, ledgerResources.Height()-1)
	metadata, err := protoutil.GetConsenterMetadataFromBlock(lastBlock)
	// Assuming a block created with cb.NewBlock(), this should not
	// error even if the orderer metadata is an empty byte slice
	if err != nil {
		return nil, errors.Wrapf(err, "error extracting orderer metadata for channel: %s", ledgerResources.ConfigtxValidator().ChannelID())
	}

	// Construct limited support needed as a parameter for additional support
	cs := &ChainSupport{
		ledgerResources:  ledgerResources,
		SignerSerializer: signer,
		cutter: blockcutter.NewReceiverImpl(
			ledgerResources.ConfigtxValidator().ChannelID(),
			ledgerResources,
			blockcutterMetrics,
		),
		BCCSP: bccsp,
	}

	// Set up the msgprocessor
	cs.Processor = msgprocessor.NewStandardChannel(cs, msgprocessor.CreateStandardChannelFilters(cs, registrar.config), bccsp)

	// Set up the block writer
	cs.BlockWriter = newBlockWriter(lastBlock, registrar, cs)

	// Set up the consenter
	consenterType := ledgerResources.SharedConfig().ConsensusType()
	consenter, ok := consenters[consenterType]
	if !ok {
		return nil, errors.Errorf("error retrieving consenter of type: %s", consenterType)
	}

	cs.Chain, err = consenter.HandleChain(cs, metadata)
	if err != nil {
		return nil, errors.Wrapf(err, "error creating consenter for channel: %s", cs.ChannelID())
	}

	cs.MetadataValidator, ok = cs.Chain.(consensus.MetadataValidator)
	if !ok {
		cs.MetadataValidator = consensus.NoOpMetadataValidator{}
	}

	cs.StatusReporter, ok = cs.Chain.(consensus.StatusReporter)
	if !ok { // Non-cluster types: solo, kafka
		cs.StatusReporter = consensus.StaticStatusReporter{ClusterRelation: types.ClusterRelationNone, Status: types.StatusActive}
	}

	logger.Debugf("[channel: %s] Done creating channel support resources", cs.ChannelID())

	return cs, nil
}

func newChainSupportForJoin(
	joinBlock *cb.Block,
	registrar *Registrar,
	ledgerResources *ledgerResources,
	consenters map[string]consensus.Consenter,
	signer identity.SignerSerializer,
	blockcutterMetrics *blockcutter.Metrics,
	bccsp bccsp.BCCSP,
) (*ChainSupport, error) {

	if joinBlock.Header.Number == 0 {
		err := ledgerResources.Append(joinBlock)
		if err != nil {
			return nil, errors.Wrap(err, "error appending join block to the ledger")
		}
		return newChainSupport(registrar, ledgerResources, consenters, signer, blockcutterMetrics, bccsp)
	}

	// Construct limited support needed as a parameter for additional support
	cs := &ChainSupport{
		ledgerResources:  ledgerResources,
		SignerSerializer: signer,
		cutter: blockcutter.NewReceiverImpl(
			ledgerResources.ConfigtxValidator().ChannelID(),
			ledgerResources,
			blockcutterMetrics,
		),
		BCCSP: bccsp,
	}

	// Set up the msgprocessor
	cs.Processor = msgprocessor.NewStandardChannel(cs, msgprocessor.CreateStandardChannelFilters(cs, registrar.config), bccsp)
	// No BlockWriter, this will be created when the chain gets converted from follower.Chain to etcdraft.Chain
	cs.BlockWriter = nil //TODO change embedding of BlockWriter struct to interface, and put here a NoOp implementation or one that panics if used

	// Get the consenter
	consenterType := ledgerResources.SharedConfig().ConsensusType()
	consenter, ok := consenters[consenterType]
	if !ok {
		return nil, errors.Errorf("error retrieving consenter of type: %s", consenterType)
	}

	var err error
	cs.Chain, err = consenter.JoinChain(cs, joinBlock)
	if err != nil {
		return nil, err
	}

	cs.MetadataValidator, ok = cs.Chain.(consensus.MetadataValidator)
	if !ok {
		cs.MetadataValidator = consensus.NoOpMetadataValidator{}
	}

	cs.StatusReporter, ok = cs.Chain.(consensus.StatusReporter)
	if !ok { // Non-cluster types: solo, kafka
		cs.StatusReporter = consensus.StaticStatusReporter{ClusterRelation: types.ClusterRelationNone, Status: types.StatusActive}
	}

	logger.Debugf("[channel: %s] Done creating channel support resources for join", cs.ChannelID())

	return cs, nil
}

// Block returns a block with the following number,
// or nil if such a block doesn't exist.
func (cs *ChainSupport) Block(number uint64) *cb.Block {
	if cs.Height() <= number {
		return nil
	}
	return blockledger.GetBlock(cs.Reader(), number)
}

func (cs *ChainSupport) Reader() blockledger.Reader {
	return cs
}

// Signer returns the SignerSerializer for this channel.
func (cs *ChainSupport) Signer() identity.SignerSerializer {
	return cs
}

func (cs *ChainSupport) start() {
	cs.Chain.Start()
}

// BlockCutter returns the blockcutter.Receiver instance for this channel.
func (cs *ChainSupport) BlockCutter() blockcutter.Receiver {
	return cs.cutter
}

// Validate passes through to the underlying configtx.Validator
func (cs *ChainSupport) Validate(configEnv *cb.ConfigEnvelope) error {
	return cs.ConfigtxValidator().Validate(configEnv)
}

// ProposeConfigUpdate validates a config update using the underlying configtx.Validator
// and the consensus.MetadataValidator.
func (cs *ChainSupport) ProposeConfigUpdate(configtx *cb.Envelope) (*cb.ConfigEnvelope, error) {
	env, err := cs.ConfigtxValidator().ProposeConfigUpdate(configtx)
	if err != nil {
		return nil, err
	}

	bundle, err := cs.CreateBundle(cs.ChannelID(), env.Config)
	if err != nil {
		return nil, err
	}

	if err = checkResources(bundle); err != nil {
		return nil, errors.Wrap(err, "config update is not compatible")
	}

	if err = cs.ValidateNew(bundle); err != nil {
		return nil, err
	}

	oldOrdererConfig, ok := cs.OrdererConfig()
	if !ok {
		logger.Panic("old config is missing orderer group")
	}
	oldMetadata := oldOrdererConfig.ConsensusMetadata()

	// we can remove this check since this is being validated in checkResources earlier
	newOrdererConfig, ok := bundle.OrdererConfig()
	if !ok {
		return nil, errors.New("new config is missing orderer group")
	}
	newMetadata := newOrdererConfig.ConsensusMetadata()

	if err = cs.ValidateConsensusMetadata(oldMetadata, newMetadata, false); err != nil {
		return nil, errors.Wrap(err, "consensus metadata update for channel config update is invalid")
	}
	return env, nil
}

// ChannelID passes through to the underlying configtx.Validator
func (cs *ChainSupport) ChannelID() string {
	return cs.ConfigtxValidator().ChannelID()
}

// ConfigProto passes through to the underlying configtx.Validator
func (cs *ChainSupport) ConfigProto() *cb.Config {
	return cs.ConfigtxValidator().ConfigProto()
}

// Sequence passes through to the underlying configtx.Validator
func (cs *ChainSupport) Sequence() uint64 {
	return cs.ConfigtxValidator().Sequence()
}

// Append appends a new block to the ledger in its raw form,
// unlike WriteBlock that also mutates its metadata.
func (cs *ChainSupport) Append(block *cb.Block) error {
	return cs.ledgerResources.ReadWriter.Append(block)
}

// VerifyBlockSignature verifies a signature of a block.
// It has an optional argument of a configuration envelope
// which would make the block verification to use validation rules
// based on the given configuration in the ConfigEnvelope.
// If the config envelope passed is nil, then the validation rules used
// are the ones that were applied at commit of previous blocks.
func (cs *ChainSupport) VerifyBlockSignature(sd []*protoutil.SignedData, envelope *cb.ConfigEnvelope) error {
	policyMgr := cs.PolicyManager()
	// If the envelope passed isn't nil, we should use a different policy manager.
	if envelope != nil {
		bundle, err := channelconfig.NewBundle(cs.ChannelID(), envelope.Config, cs.BCCSP)
		if err != nil {
			return err
		}
		policyMgr = bundle.PolicyManager()
	}
	policy, exists := policyMgr.GetPolicy(policies.BlockValidation)
	if !exists {
		return errors.Errorf("policy %s wasn't found", policies.BlockValidation)
	}
	err := policy.EvaluateSignedData(sd)
	if err != nil {
		return errors.Wrap(err, "block verification failed")
	}
	return nil
}
