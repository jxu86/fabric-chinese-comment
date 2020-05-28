/*
Copyright IBM Corp. 2018 All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package endorser

import (
	"fmt"
	"sync"

	"github.com/hyperledger/fabric/core/chaincode/shim"
	endorsement3 "github.com/hyperledger/fabric/core/handlers/endorsement/api/identities"
	"github.com/hyperledger/fabric/core/transientstore"
	pb "github.com/hyperledger/fabric/protos/peer"
	putils "github.com/hyperledger/fabric/protos/utils"
	"github.com/pkg/errors"
)

//go:generate mockery -dir . -name TransientStoreRetriever -case underscore -output mocks/
//go:generate mockery -dir ../transientstore/ -name Store -case underscore -output mocks/

// TransientStoreRetriever retrieves transient stores
type TransientStoreRetriever interface {
	// StoreForChannel returns the transient store for the given channel
	StoreForChannel(channel string) transientstore.Store
}

//go:generate mockery -dir . -name ChannelStateRetriever -case underscore -output mocks/

// ChannelStateRetriever retrieves Channel state
type ChannelStateRetriever interface {
	// ChannelState returns a QueryCreator for the given Channel
	NewQueryCreator(channel string) (QueryCreator, error)
}

//go:generate mockery -dir . -name PluginMapper -case underscore -output mocks/

// PluginMapper maps plugin names to their corresponding factories
type PluginMapper interface {
	PluginFactoryByName(name PluginName) endorsement.PluginFactory
}

// MapBasedPluginMapper maps plugin names to their corresponding factories
type MapBasedPluginMapper map[string]endorsement.PluginFactory

// PluginFactoryByName returns a plugin factory for the given plugin name, or nil if not found
func (m MapBasedPluginMapper) PluginFactoryByName(name PluginName) endorsement.PluginFactory {
	return m[string(name)]
}

// Context defines the data that is related to an in-flight endorsement
type Context struct {
	PluginName     string
	Channel        string
	TxID           string
	Proposal       *pb.Proposal
	SignedProposal *pb.SignedProposal
	Visibility     []byte
	Response       *pb.Response
	Event          []byte
	ChaincodeID    *pb.ChaincodeID
	SimRes         []byte
}

// String returns a text representation of this context
func (c Context) String() string {
	return fmt.Sprintf("{plugin: %s, channel: %s, tx: %s, chaincode: %s}", c.PluginName, c.Channel, c.TxID, c.ChaincodeID.Name)
}

// PluginSupport aggregates the support interfaces
// needed for the operation of the plugin endorser
type PluginSupport struct {
	ChannelStateRetriever
	endorsement3.SigningIdentityFetcher
	PluginMapper
	TransientStoreRetriever
}

// NewPluginEndorser endorses with using a plugin
func NewPluginEndorser(ps *PluginSupport) *PluginEndorser {
	return &PluginEndorser{
		SigningIdentityFetcher:  ps.SigningIdentityFetcher,
		PluginMapper:            ps.PluginMapper,
		pluginChannelMapping:    make(map[PluginName]*pluginsByChannel),
		ChannelStateRetriever:   ps.ChannelStateRetriever,
		TransientStoreRetriever: ps.TransientStoreRetriever,
	}
}

// PluginName defines the name of the plugin as it appears in the configuration
type PluginName string

type pluginsByChannel struct {
	sync.RWMutex
	pluginFactory    endorsement.PluginFactory
	channels2Plugins map[string]endorsement.Plugin
	pe               *PluginEndorser
}

func (pbc *pluginsByChannel) createPluginIfAbsent(channel string) (endorsement.Plugin, error) {
	// 首先就是获取一个读锁
	pbc.RLock()
	// 根据数组下标找需要的插件
	plugin, exists := pbc.channels2Plugins[channel]
	// 释放读锁
	pbc.RUnlock()
	// 如果找到的话直接返回
	if exists {
		return plugin, nil
	}
	// 到这里说明没有找到，表明插件不存在，这次获取锁，这是与上面的锁不同
	pbc.Lock()
	defer pbc.Unlock()
	// 再进行一次查找，多线程下说不定有其他线程刚刚创建了呢
	plugin, exists = pbc.channels2Plugins[channel]
	// 如果查找到的话释放锁后直接返回
	if exists {
		return plugin, nil
	}
	// 到这里说明真的没有该插件，使用插件工厂New一个
	pluginInstance := pbc.pluginFactory.New()
	// 进行初始化操作
	plugin, err := pbc.initPlugin(pluginInstance, channel)
	if err != nil {
		return nil, err
	}
	// 添加到数组里，下次再查找该插件的时候就存在了
	pbc.channels2Plugins[channel] = plugin
	// 最后释放锁后返回
	return plugin, nil
}

func (pbc *pluginsByChannel) initPlugin(plugin endorsement.Plugin, channel string) (endorsement.Plugin, error) {
	var dependencies []endorsement.Dependency
	var err error
	// If this is a channel endorsement, add the channel state as a dependency
	if channel != "" {
		// 根据给予的通道信息创建一个用于查询的Creator
		query, err := pbc.pe.NewQueryCreator(channel)
		if err != nil {
			return nil, errors.Wrap(err, "failed obtaining channel state")
		}
		// 根据给予的通道信息获取状态数据，也就是当前账本中最新状态
		store := pbc.pe.TransientStoreRetriever.StoreForChannel(channel)
		if store == nil {
			return nil, errors.Errorf("transient store for channel %s was not initialized", channel)
		}
		// 添加进数组中
		dependencies = append(dependencies, &ChannelState{QueryCreator: query, Store: store})
	}
	// Add the SigningIdentityFetcher as a dependency
	dependencies = append(dependencies, pbc.pe.SigningIdentityFetcher)
	//Plugin的初始化方法在这里被调用
	err = plugin.Init(dependencies...)
	if err != nil {
		return nil, err
	}
	return plugin, nil
}

// PluginEndorser endorsers proposal responses using plugins
type PluginEndorser struct {
	sync.Mutex
	PluginMapper
	pluginChannelMapping map[PluginName]*pluginsByChannel
	ChannelStateRetriever
	endorsement3.SigningIdentityFetcher
	TransientStoreRetriever
}

// EndorseWithPlugin endorses the response with a plugin
func (pe *PluginEndorser) EndorseWithPlugin(ctx Context) (*pb.ProposalResponse, error) {
	endorserLogger.Debug("Entering endorsement for", ctx)

	if ctx.Response == nil {
		return nil, errors.New("response is nil")
	}

	if ctx.Response.Status >= shim.ERRORTHRESHOLD {
		return &pb.ProposalResponse{Response: ctx.Response}, nil
	}
	// 获取或者创建插件
	plugin, err := pe.getOrCreatePlugin(PluginName(ctx.PluginName), ctx.Channel)
	if err != nil {
		endorserLogger.Warning("Endorsement with plugin for", ctx, " failed:", err)
		return nil, errors.Errorf("plugin with name %s could not be used: %v", ctx.PluginName, err)
	}
	// 从上下文中获取提案byte数据
	prpBytes, err := proposalResponsePayloadFromContext(ctx)
	if err != nil {
		endorserLogger.Warning("Endorsement with plugin for", ctx, " failed:", err)
		return nil, errors.Wrap(err, "failed assembling proposal response payload")
	}
	// 进行背书操作
	endorsement, prpBytes, err := plugin.Endorse(prpBytes, ctx.SignedProposal)
	if err != nil {
		endorserLogger.Warning("Endorsement with plugin for", ctx, " failed:", err)
		return nil, errors.WithStack(err)
	}
	// 背书完成后，封装为提案响应结构体，最后将该结构体返回
	resp := &pb.ProposalResponse{
		Version:     1,
		Endorsement: endorsement,
		Payload:     prpBytes,
		Response:    ctx.Response,
	}
	endorserLogger.Debug("Exiting", ctx)
	return resp, nil
}

// getAndStorePlugin returns a plugin instance for the given plugin name and channel
func (pe *PluginEndorser) getOrCreatePlugin(plugin PluginName, channel string) (endorsement.Plugin, error) {
	// 获取插件工厂
	pluginFactory := pe.PluginFactoryByName(plugin)
	if pluginFactory == nil {
		return nil, errors.Errorf("plugin with name %s wasn't found", plugin)
	}
	// 这个就是获取或创建一个通道映射，意思就是如果有就直接获取，没有就先创建再获取
	pluginsByChannel := pe.getOrCreatePluginChannelMapping(PluginName(plugin), pluginFactory)
	// 根据通道创建插件
	return pluginsByChannel.createPluginIfAbsent(channel)
}

func (pe *PluginEndorser) getOrCreatePluginChannelMapping(plugin PluginName, pf endorsement.PluginFactory) *pluginsByChannel {
	pe.Lock()
	defer pe.Unlock()
	endorserChannelMapping, exists := pe.pluginChannelMapping[PluginName(plugin)]
	if !exists {
		endorserChannelMapping = &pluginsByChannel{
			pluginFactory:    pf,
			channels2Plugins: make(map[string]endorsement.Plugin),
			pe:               pe,
		}
		pe.pluginChannelMapping[PluginName(plugin)] = endorserChannelMapping
	}
	return endorserChannelMapping
}

func proposalResponsePayloadFromContext(ctx Context) ([]byte, error) {
	hdr, err := putils.GetHeader(ctx.Proposal.Header)
	if err != nil {
		endorserLogger.Warning("Failed parsing header", err)
		return nil, errors.Wrap(err, "failed parsing header")
	}

	pHashBytes, err := putils.GetProposalHash1(hdr, ctx.Proposal.Payload, ctx.Visibility)
	if err != nil {
		endorserLogger.Warning("Failed computing proposal hash", err)
		return nil, errors.Wrap(err, "could not compute proposal hash")
	}

	prpBytes, err := putils.GetBytesProposalResponsePayload(pHashBytes, ctx.Response, ctx.SimRes, ctx.Event, ctx.ChaincodeID)
	if err != nil {
		endorserLogger.Warning("Failed marshaling the proposal response payload to bytes", err)
		return nil, errors.New("failure while marshaling the ProposalResponsePayload")
	}
	return prpBytes, nil
}
