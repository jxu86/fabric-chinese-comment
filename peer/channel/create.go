/*
Copyright IBM Corp. 2017 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

                 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package channel

import (
	"fmt"
	"io/ioutil"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/common/configtx"
	localsigner "github.com/hyperledger/fabric/common/localmsp"
	"github.com/hyperledger/fabric/common/tools/configtxgen/encoder"
	genesisconfig "github.com/hyperledger/fabric/common/tools/configtxgen/localconfig"
	"github.com/hyperledger/fabric/common/util"
	"github.com/hyperledger/fabric/peer/common"
	cb "github.com/hyperledger/fabric/protos/common"
	"github.com/hyperledger/fabric/protos/utils"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

//ConfigTxFileNotFound channel create configuration tx file not found
type ConfigTxFileNotFound string

func (e ConfigTxFileNotFound) Error() string {
	return fmt.Sprintf("channel create configuration tx file not found %s", string(e))
}

//InvalidCreateTx invalid channel create transaction
type InvalidCreateTx string

func (e InvalidCreateTx) Error() string {
	return fmt.Sprintf("Invalid channel create transaction : %s", string(e))
}

func createCmd(cf *ChannelCmdFactory) *cobra.Command {
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a channel",
		Long:  "Create a channel and write the genesis block to a file.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return create(cmd, args, cf)
		},
	}
	flagList := []string{
		"channelID",
		"file",
		"outputBlock",
		"timeout",
	}
	attachFlags(createCmd, flagList)

	return createCmd
}

func createChannelFromDefaults(cf *ChannelCmdFactory) (*cb.Envelope, error) {
	chCrtEnv, err := encoder.MakeChannelCreationTransaction(channelID, localsigner.NewSigner(), genesisconfig.Load(genesisconfig.SampleSingleMSPChannelProfile))
	if err != nil {
		return nil, err
	}

	return chCrtEnv, nil
}

func createChannelFromConfigTx(configTxFileName string) (*cb.Envelope, error) {
	cftx, err := ioutil.ReadFile(configTxFileName)
	if err != nil {
		return nil, ConfigTxFileNotFound(err.Error())
	}

	return utils.UnmarshalEnvelope(cftx)
}

func sanityCheckAndSignConfigTx(envConfigUpdate *cb.Envelope) (*cb.Envelope, error) {
	payload, err := utils.ExtractPayload(envConfigUpdate)
	if err != nil {
		return nil, InvalidCreateTx("bad payload")
	}

	if payload.Header == nil || payload.Header.ChannelHeader == nil {
		return nil, InvalidCreateTx("bad header")
	}

	ch, err := utils.UnmarshalChannelHeader(payload.Header.ChannelHeader)
	if err != nil {
		return nil, InvalidCreateTx("could not unmarshall channel header")
	}

	if ch.Type != int32(cb.HeaderType_CONFIG_UPDATE) {
		return nil, InvalidCreateTx("bad type")
	}

	if ch.ChannelId == "" {
		return nil, InvalidCreateTx("empty channel id")
	}

	// Specifying the chainID on the CLI is usually redundant, as a hack, set it
	// here if it has not been set explicitly
	if channelID == "" {
		channelID = ch.ChannelId
	}

	if ch.ChannelId != channelID {
		return nil, InvalidCreateTx(fmt.Sprintf("mismatched channel ID %s != %s", ch.ChannelId, channelID))
	}

	configUpdateEnv, err := configtx.UnmarshalConfigUpdateEnvelope(payload.Data)
	if err != nil {
		return nil, InvalidCreateTx("Bad config update env")
	}

	signer := localsigner.NewSigner()
	sigHeader, err := signer.NewSignatureHeader()
	if err != nil {
		return nil, err
	}

	configSig := &cb.ConfigSignature{
		SignatureHeader: utils.MarshalOrPanic(sigHeader),
	}

	configSig.Signature, err = signer.Sign(util.ConcatenateBytes(configSig.SignatureHeader, configUpdateEnv.ConfigUpdate))

	configUpdateEnv.Signatures = append(configUpdateEnv.Signatures, configSig)

	return utils.CreateSignedEnvelope(cb.HeaderType_CONFIG_UPDATE, channelID, signer, configUpdateEnv, 0, 0)
}

func sendCreateChainTransaction(cf *ChannelCmdFactory) error {
	var err error
	// 定义了一个Envelope结构体
	var chCrtEnv *cb.Envelope

	if channelTxFile != "" {
		// 如果指定了channelTxFile，则使用指定的文件创建通道，这个方法很简单，从文件中读取数据，
		// 反序列化后返回chCrtEnv.对于我们启动Fabric网络之前曾创建过一个channel.tx文件，指的就是这个
		if chCrtEnv, err = createChannelFromConfigTx(channelTxFile); err != nil {
			return err
		}
	} else {
		// 如果没有指定，则使用默认的配置创建通道
		if chCrtEnv, err = createChannelFromDefaults(cf); err != nil {
			return err
		}
	}
	// 该方法主要是对刚刚创建的Envelope进行验证
	if chCrtEnv, err = sanityCheckAndSignConfigTx(chCrtEnv); err != nil {
		return err
	}

	var broadcastClient common.BroadcastClient
	// 创建一个用于广播信息的客户端
	broadcastClient, err = cf.BroadcastFactory()
	if err != nil {
		return errors.WithMessage(err, "error getting broadcast client")
	}

	defer broadcastClient.Close()
	// 将创建通道的Envelope信息广播出去
	err = broadcastClient.Send(chCrtEnv)

	return err
}

func executeCreate(cf *ChannelCmdFactory) error {
	// 发送创建通道的Transaction到Order节点
	err := sendCreateChainTransaction(cf)
	if err != nil {
		return err
	}
	// 获取该通道内的创世区块(该过程在Order节点共识完成之后)
	block, err := getGenesisBlock(cf)
	if err != nil {
		return err
	}
	// 序列化区块信息
	b, err := proto.Marshal(block)
	if err != nil {
		return err
	}

	file := channelID + ".block"
	if outputBlock != common.UndefinedParamValue {
		file = outputBlock
	}
	// 将区块信息写入本地文件中
	err = ioutil.WriteFile(file, b, 0644)
	if err != nil {
		return err
	}

	return nil
}

func getGenesisBlock(cf *ChannelCmdFactory) (*cb.Block, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			cf.DeliverClient.Close()
			return nil, errors.New("timeout waiting for channel creation")
		default:
			if block, err := cf.DeliverClient.GetSpecifiedBlock(0); err != nil {
				cf.DeliverClient.Close()
				cf, err = InitCmdFactory(EndorserNotRequired, PeerDeliverNotRequired, OrdererRequired)
				if err != nil {
					return nil, errors.WithMessage(err, "failed connecting")
				}
				time.Sleep(200 * time.Millisecond)
			} else {
				cf.DeliverClient.Close()
				return block, nil
			}
		}
	}
}

func create(cmd *cobra.Command, args []string, cf *ChannelCmdFactory) error {
	// the global chainID filled by the "-c" command
	if channelID == common.UndefinedParamValue {
		return errors.New("must supply channel ID")
	}

	// Parsing of the command line is done so silence cmd usage
	cmd.SilenceUsage = true

	var err error
	if cf == nil {
		// 初始化ChannelCmdFactory
		cf, err = InitCmdFactory(EndorserNotRequired, PeerDeliverNotRequired, OrdererRequired)
		if err != nil {
			return err
		}
	}
	return executeCreate(cf)
}
