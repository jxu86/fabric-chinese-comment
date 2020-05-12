/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package chaincode

import (
	"context"
	"fmt"
	"io/ioutil"

	"github.com/pkg/errors"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/core/common/ccpackage"
	"github.com/hyperledger/fabric/core/common/ccprovider"
	"github.com/hyperledger/fabric/peer/common"
	pcommon "github.com/hyperledger/fabric/protos/common"
	pb "github.com/hyperledger/fabric/protos/peer"
	"github.com/hyperledger/fabric/protos/utils"
	"github.com/spf13/cobra"
)

var chaincodeInstallCmd *cobra.Command

const installCmdName = "install"

const installDesc = "Package the specified chaincode into a deployment spec and save it on the peer's path."

// installCmd returns the cobra command for Chaincode Deploy
func installCmd(cf *ChaincodeCmdFactory) *cobra.Command {
	chaincodeInstallCmd = &cobra.Command{
		Use:       "install",
		Short:     fmt.Sprint(installDesc),
		Long:      fmt.Sprint(installDesc),
		ValidArgs: []string{"1"},
		RunE: func(cmd *cobra.Command, args []string) error {
			var ccpackfile string
			if len(args) > 0 {
				ccpackfile = args[0]
			}
			return chaincodeInstall(cmd, ccpackfile, cf)
		},
	}
	flagList := []string{	// 在安装链码的命令中指定的相关参数
		"lang",
		"ctor",
		"path",
		"name",
		"version",
		"peerAddresses",
		"tlsRootCertFiles",
		"connectionProfile",
	}
	attachFlags(chaincodeInstallCmd, flagList)

	return chaincodeInstallCmd
}

//install the depspec to "peer.address"
func install(msg proto.Message, cf *ChaincodeCmdFactory) error {
	// 首先获取一个用于发起提案与签名的creator
	creator, err := cf.Signer.Serialize()
	if err != nil {
		return fmt.Errorf("Error serializing identity for %s: %s", cf.Signer.GetIdentifier(), err)
	}
	// 从ChaincodeDeploymentSpec中创建一个用于安装链码的Proposal
	prop, _, err := utils.CreateInstallProposalFromCDS(msg, creator)
	if err != nil {
		return fmt.Errorf("Error creating proposal  %s: %s", chainFuncName, err)
	}

	var signedProp *pb.SignedProposal
	// 对创建的Proposal进行签名
	signedProp, err = utils.GetSignedProposal(prop, cf.Signer)
	if err != nil {
		return fmt.Errorf("Error creating signed proposal  %s: %s", chainFuncName, err)
	}

	// install is currently only supported for one peer
	// 这里安装链码只在指定的Peer节点，而不是所有Peer节点，依旧是调用了主要的方法ProcessProposal
	proposalResponse, err := cf.EndorserClients[0].ProcessProposal(context.Background(), signedProp)
	// 到这里，Peer节点对提案处理完成之后，整个链码安装的过程就结束了
	if err != nil {
		return fmt.Errorf("Error endorsing %s: %s", chainFuncName, err)
	}

	if proposalResponse != nil {
		if proposalResponse.Response.Status != int32(pcommon.Status_SUCCESS) {
			return errors.Errorf("Bad response: %d - %s", proposalResponse.Response.Status, proposalResponse.Response.Message)
		}
		logger.Infof("Installed remotely %v", proposalResponse)
	} else {
		return errors.New("Error during install: received nil proposal response")
	}

	return nil
}

//genChaincodeDeploymentSpec creates ChaincodeDeploymentSpec as the package to install
func genChaincodeDeploymentSpec(cmd *cobra.Command, chaincodeName, chaincodeVersion string) (*pb.ChaincodeDeploymentSpec, error) {
	// 首先根据链码名称与链码版本查找当前链码是否已经安装过，如果安装过则返回链码已存在的错误
	if existed, _ := ccprovider.ChaincodePackageExists(chaincodeName, chaincodeVersion); existed {
		return nil, fmt.Errorf("chaincode %s:%s already exists", chaincodeName, chaincodeVersion)
	}
	// 获取链码标准数据结构
	spec, err := getChaincodeSpec(cmd)
	if err != nil {
		return nil, err
	}
	// 获取链码部署标准数据结构
	cds, err := getChaincodeDeploymentSpec(spec, true)
	if err != nil {
		return nil, fmt.Errorf("error getting chaincode code %s: %s", chaincodeName, err)
	}

	return cds, nil
}

//getPackageFromFile get the chaincode package from file and the extracted ChaincodeDeploymentSpec
func getPackageFromFile(ccpackfile string) (proto.Message, *pb.ChaincodeDeploymentSpec, error) {
	b, err := ioutil.ReadFile(ccpackfile)
	if err != nil {
		return nil, nil, err
	}

	//the bytes should be a valid package (CDS or SignedCDS)
	ccpack, err := ccprovider.GetCCPackage(b)
	if err != nil {
		return nil, nil, err
	}

	//either CDS or Envelope
	o := ccpack.GetPackageObject()

	//try CDS first
	cds, ok := o.(*pb.ChaincodeDeploymentSpec)
	if !ok || cds == nil {
		//try Envelope next
		env, ok := o.(*pcommon.Envelope)
		if !ok || env == nil {
			return nil, nil, fmt.Errorf("error extracting valid chaincode package")
		}

		//this will check for a valid package Envelope
		_, sCDS, err := ccpackage.ExtractSignedCCDepSpec(env)
		if err != nil {
			return nil, nil, fmt.Errorf("error extracting valid signed chaincode package(%s)", err)
		}

		//...and get the CDS at last
		cds, err = utils.GetChaincodeDeploymentSpec(sCDS.ChaincodeDeploymentSpec, platformRegistry)
		if err != nil {
			return nil, nil, fmt.Errorf("error extracting chaincode deployment spec(%s)", err)
		}
	}

	return o, cds, nil
}

// chaincodeInstall installs the chaincode. If remoteinstall, does it via a lscc call
func chaincodeInstall(cmd *cobra.Command, ccpackfile string, cf *ChaincodeCmdFactory) error {
	// Parsing of the command line is done so silence cmd usage
	cmd.SilenceUsage = true

	var err error
	if cf == nil {
		// 如果ChaincodeCmdFactory为空，则初始化一个
		cf, err = InitCmdFactory(cmd.Name(), true, false)
		if err != nil {
			return err
		}
	}

	var ccpackmsg proto.Message
	// 这个地方有两种情况，链码可能是根据传入参数从本地链码源代码文件读取，也有可能是由其他节点签名打包完成发送过来的
	if ccpackfile == "" {
		// 这里是从本地链码源代码文件读取
		if chaincodePath == common.UndefinedParamValue || chaincodeVersion == common.UndefinedParamValue || chaincodeName == common.UndefinedParamValue {
			return fmt.Errorf("Must supply value for %s name, path and version parameters.", chainFuncName)
		}
		//generate a raw ChaincodeDeploymentSpec
		// 生成ChaincodeDeploymentSpce
		ccpackmsg, err = genChaincodeDeploymentSpec(cmd, chaincodeName, chaincodeVersion)
		if err != nil {
			return err
		}
	} else {
		//read in a package generated by the "package" sub-command (and perhaps signed
		//by multiple owners with the "signpackage" sub-command)
		// 首先从ccpackfile中获取数据，主要就是从文件中读取已定义的ChaincodeDeploymentSpec
		var cds *pb.ChaincodeDeploymentSpec
		ccpackmsg, cds, err = getPackageFromFile(ccpackfile)

		if err != nil {
			return err
		}

		//get the chaincode details from cds
		// 由于ccpackfile中已经定义完成了以上的数据结构，所以这里就直接获取了
		cName := cds.ChaincodeSpec.ChaincodeId.Name
		cVersion := cds.ChaincodeSpec.ChaincodeId.Version

		//if user provided chaincodeName, use it for validation
		if chaincodeName != "" && chaincodeName != cName {
			return fmt.Errorf("chaincode name %s does not match name %s in package", chaincodeName, cName)
		}

		//if user provided chaincodeVersion, use it for validation
		if chaincodeVersion != "" && chaincodeVersion != cVersion {
			return fmt.Errorf("chaincode version %s does not match version %s in packages", chaincodeVersion, cVersion)
		}
	}
	// 链码安装
	err = install(ccpackmsg, cf)

	return err
}
