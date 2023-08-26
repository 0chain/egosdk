package zcnbridge

import (
	"context"
	"encoding/json"
	"log"
	"math/big"
	"os"
	"strconv"
	"testing"
	"time"

	sdkcommon "github.com/0chain/gosdk/core/common"
	"github.com/0chain/gosdk/zcnbridge/ethereum"
	"github.com/0chain/gosdk/zcnbridge/ethereum/authorizers"
	binding "github.com/0chain/gosdk/zcnbridge/ethereum/bridge"
	"github.com/0chain/gosdk/zcnbridge/ethereum/erc20"
	bridgemocks "github.com/0chain/gosdk/zcnbridge/mocks"
	"github.com/0chain/gosdk/zcnbridge/transaction"
	transactionmocks "github.com/0chain/gosdk/zcnbridge/transaction/mocks"
	"github.com/0chain/gosdk/zcnbridge/wallet"
	"github.com/0chain/gosdk/zcnbridge/zcnsc"
	"github.com/0chain/gosdk/zcncore"
	eth "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	ethereumMnemonic = "symbol alley celery diesel donate moral almost opinion achieve since diamond page"

	ethereumAddress = "0xD8c9156e782C68EE671C09b6b92de76C97948432"
	password        = "\"02289b9\""

	authorizerDelegatedAddress = "0xa149B58b7e1390D152383BB03dBc79B390F648e2"

	bridgeAddress     = "0x7bbbEa24ac1751317D7669f05558632c4A9113D7"
	tokenAddress      = "0x2ec8F26ccC678c9faF0Df20208aEE3AF776160CD"
	authorizerAddress = "0xEAe8229c0E457efBA1A1769e7F8c20110fF68E61"

	zcnTxnID = "b26abeb31fcee5d2e75b26717722938a06fa5ce4a5b5e68ddad68357432caace"
	amount   = 1e10
	txnFee   = 1
	nonce    = 1

	ethereumTxnID = "0x3b59971c2aa294739cd73912f0c5a7996aafb796238cf44408b0eb4af0fbac82"

	clientId = "d6e9b3222434faa043c683d1a939d6a0fa2818c4d56e794974d64a32005330d3"
)

var (
	ethereumSignatures = []*ethereum.AuthorizerSignature{
		{
			ID:        "0x2ec8F26ccC678c9faF0Df20208aEE3AF776160CD",
			Signature: []byte("0xEAe8229c0E457efBA1A1769e7F8c20110fF68E61"),
		},
	}

	zcnScSignatures = []*zcnsc.AuthorizerSignature{
		{
			ID:        "0x2ec8F26ccC678c9faF0Df20208aEE3AF776160CD",
			Signature: "0xEAe8229c0E457efBA1A1769e7F8c20110fF68E61",
		},
	}
)

type ethereumClientMock struct {
	mock.TestingT
}

func (ecm *ethereumClientMock) Cleanup(callback func()) {
	callback()
}

type transactionMock struct {
	mock.TestingT
}

func (tem *transactionMock) Cleanup(callback func()) {
	callback()
}

type transactionProviderMock struct {
	mock.TestingT
}

func (tem *transactionProviderMock) Cleanup(callback func()) {
	callback()
}

type keyStoreMock struct {
	mock.TestingT
}

func (ksm *keyStoreMock) Cleanup(callback func()) {
	callback()
}

type authorizerConfigTarget struct {
	Fee sdkcommon.Balance `json:"fee"`
}

type authorizerNodeTarget struct {
	ID        string                  `json:"id"`
	PublicKey string                  `json:"public_key"`
	URL       string                  `json:"url"`
	Config    *authorizerConfigTarget `json:"config"`
}

type authorizerConfigSource struct {
	Fee string `json:"fee"`
}

type authorizerNodeSource struct {
	ID     string                  `json:"id"`
	Config *authorizerConfigSource `json:"config"`
}

func (an *authorizerNodeTarget) decode(input []byte) error {
	var objMap map[string]*json.RawMessage
	err := json.Unmarshal(input, &objMap)
	if err != nil {
		return err
	}

	id, ok := objMap["id"]
	if ok {
		var idStr *string
		err = json.Unmarshal(*id, &idStr)
		if err != nil {
			return err
		}
		an.ID = *idStr
	}

	pk, ok := objMap["public_key"]
	if ok {
		var pkStr *string
		err = json.Unmarshal(*pk, &pkStr)
		if err != nil {
			return err
		}
		an.PublicKey = *pkStr
	}

	url, ok := objMap["url"]
	if ok {
		var urlStr *string
		err = json.Unmarshal(*url, &urlStr)
		if err != nil {
			return err
		}
		an.URL = *urlStr
	}

	rawCfg, ok := objMap["config"]
	if ok {
		var cfg = &authorizerConfigTarget{}
		err = cfg.decode(*rawCfg)
		if err != nil {
			return err
		}

		an.Config = cfg
	}

	return nil
}

func (c *authorizerConfigTarget) decode(input []byte) (err error) {
	const (
		Fee = "fee"
	)

	var objMap map[string]*json.RawMessage
	err = json.Unmarshal(input, &objMap)
	if err != nil {
		return err
	}

	fee, ok := objMap[Fee]
	if ok {
		var feeStr *string
		err = json.Unmarshal(*fee, &feeStr)
		if err != nil {
			return err
		}

		var balance, err = strconv.ParseInt(*feeStr, 10, 64)
		if err != nil {
			return err
		}

		c.Fee = sdkcommon.Balance(balance)
	}

	return nil
}

func getEthereumClient(t mock.TestingT) *bridgemocks.EthereumClient {
	return bridgemocks.NewEthereumClient(&ethereumClientMock{t})
}

func getBridgeClient(ethereumClient EthereumClient, transactionProvider transaction.TransactionProvider, keyStore KeyStore) *BridgeClient {
	cfg := viper.New()

	tempConfigFile, err := os.CreateTemp(".", "config.yaml")
	if err != nil {
		log.Fatalln(err)
	}

	defer os.Remove(tempConfigFile.Name())

	cfg.SetConfigFile(tempConfigFile.Name())

	cfg.SetDefault("bridge.bridge_address", bridgeAddress)
	cfg.SetDefault("bridge.token_address", tokenAddress)
	cfg.SetDefault("bridge.authorizers_address", authorizerAddress)
	cfg.SetDefault("bridge.ethereum_address", ethereumAddress)
	cfg.SetDefault("bridge.password", password)
	cfg.SetDefault("bridge.gas_limit", 0)
	cfg.SetDefault("bridge.consensus_threshold", 0)

	return createBridgeClient(cfg, ethereumClient, transactionProvider, keyStore)
}

func prepareEthereumClientGeneralMockCalls(ethereumClient *mock.Mock) {
	ethereumClient.On("EstimateGas", mock.Anything, mock.Anything).Return(uint64(400000), nil)
	ethereumClient.On("ChainID", mock.Anything).Return(big.NewInt(400000), nil)
	ethereumClient.On("PendingNonceAt", mock.Anything, mock.Anything).Return(uint64(nonce), nil)
	ethereumClient.On("SuggestGasPrice", mock.Anything).Return(big.NewInt(400000), nil)
	ethereumClient.On("SendTransaction", mock.Anything, mock.Anything).Return(nil)
}

func getTransaction(t mock.TestingT) *transactionmocks.Transaction {
	return transactionmocks.NewTransaction(&transactionMock{t})
}

func prepareTransactionGeneralMockCalls(transaction *mock.Mock) {
	transaction.On("ExecuteSmartContract", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(zcnTxnID, nil)
	transaction.On("Verify", mock.Anything).Return(nil)
}

func getTransactionProvider(t mock.TestingT) *transactionmocks.TransactionProvider {
	return transactionmocks.NewTransactionProvider(&transactionProviderMock{t})
}

func prepareTransactionProviderGeneralMockCalls(transactionProvider *mock.Mock, transaction *transactionmocks.Transaction) {
	transactionProvider.On("NewTransactionEntity", mock.Anything).Return(transaction, nil)
}

func getKeyStore(t mock.TestingT) *bridgemocks.KeyStore {
	return bridgemocks.NewKeyStore(&keyStoreMock{t})
}

func prepareKeyStoreGeneralMockCalls(keyStore *bridgemocks.KeyStore) {
	keyStore.On("Find", mock.Anything).Return(accounts.Account{
		Address: common.HexToAddress(ethereumAddress),
	}, nil)
	keyStore.On("TimedUnlock", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	keyStore.On("SignHash", mock.Anything, mock.Anything).Return([]byte(ethereumAddress), nil)

	keyStoreDir, err := os.MkdirTemp(".", "keyStore")
	if err != nil {
		log.Fatalln(err)
	}

	defer os.Remove(keyStoreDir)

	ks := keystore.NewKeyStore(keyStoreDir, keystore.StandardScryptN, keystore.StandardScryptP)

	// _, err = ImportAccount(keyStoreDir, ethereumMnemonic, password)
	// if err != nil {
	// 	log.Fatalln(err)
	// }

	keyStore.On("GetEthereumKeyStore").Return(ks)
}

func Test_ZCNBridge(t *testing.T) {
	ethereumClient := getEthereumClient(t)
	prepareEthereumClientGeneralMockCalls(&ethereumClient.Mock)

	transaction := getTransaction(t)
	prepareTransactionGeneralMockCalls(&transaction.Mock)

	transactionProvider := getTransactionProvider(t)
	prepareTransactionProviderGeneralMockCalls(&transactionProvider.Mock, transaction)

	keyStore := getKeyStore(t)
	prepareKeyStoreGeneralMockCalls(keyStore)

	bridgeClient := getBridgeClient(ethereumClient, transactionProvider, keyStore)

	t.Run("should update authorizer config.", func(t *testing.T) {
		source := &authorizerNodeSource{
			ID: "12345678",
			Config: &authorizerConfigSource{
				Fee: "999",
			},
		}
		target := &authorizerNodeTarget{}

		bytes, err := json.Marshal(source)
		require.NoError(t, err)

		err = target.decode(bytes)
		require.NoError(t, err)

		require.Equal(t, "", target.URL)
		require.Equal(t, "", target.PublicKey)
		require.Equal(t, "12345678", target.ID)
		require.Equal(t, sdkcommon.Balance(999), target.Config.Fee)
	})

	t.Run("should check configuration formating in MintWZCN", func(t *testing.T) {
		_, err := bridgeClient.MintWZCN(context.Background(), &ethereum.MintPayload{
			ZCNTxnID:   zcnTxnID,
			Amount:     amount,
			To:         ethereumAddress,
			Nonce:      nonce,
			Signatures: ethereumSignatures,
		})
		require.NoError(t, err)

		var sigs [][]byte
		for _, signature := range ethereumSignatures {
			sigs = append(sigs, signature.Signature)
		}

		to := common.HexToAddress(bridgeAddress)
		fromAddress := common.HexToAddress(ethereumAddress)

		abi, err := binding.BridgeMetaData.GetAbi()
		require.NoError(t, err)

		pack, err := abi.Pack("mint", common.HexToAddress(ethereumAddress),
			big.NewInt(amount),
			DefaultClientIDEncoder(zcnTxnID),
			big.NewInt(nonce),
			sigs)
		require.NoError(t, err)

		require.True(t, ethereumClient.AssertCalled(
			t,
			"EstimateGas",
			context.Background(),
			eth.CallMsg{
				To:   &to,
				From: fromAddress,
				Data: pack,
			},
		))
	})

	t.Run("should check configuration formating in BurnWZCN", func(t *testing.T) {
		_, err := bridgeClient.BurnWZCN(context.Background(), amount)
		require.NoError(t, err)

		to := common.HexToAddress(bridgeAddress)
		fromAddress := common.HexToAddress(ethereumAddress)

		abi, err := binding.BridgeMetaData.GetAbi()
		require.NoError(t, err)

		pack, err := abi.Pack("burn", big.NewInt(amount), DefaultClientIDEncoder(zcncore.GetClientWalletID()))
		require.NoError(t, err)

		require.True(t, ethereumClient.AssertCalled(
			t,
			"EstimateGas",
			context.Background(),
			eth.CallMsg{
				To:   &to,
				From: fromAddress,
				Data: pack,
			},
		))
	})

	t.Run("should check configuration used by MintZCN", func(t *testing.T) {
		payload := &zcnsc.MintPayload{
			EthereumTxnID:     ethereumTxnID,
			Amount:            sdkcommon.Balance(amount),
			Nonce:             nonce,
			Signatures:        zcnScSignatures,
			ReceivingClientID: clientId,
		}

		_, err := bridgeClient.MintZCN(context.Background(), payload)
		require.NoError(t, err)

		require.True(t, transaction.AssertCalled(
			t,
			"ExecuteSmartContract",
			context.Background(),
			wallet.ZCNSCSmartContractAddress,
			wallet.MintFunc,
			payload,
			uint64(0),
		))
	})

	t.Run("should check configuration used by BurnZCN", func(t *testing.T) {
		_, err := bridgeClient.BurnZCN(context.Background(), amount, txnFee)
		require.NoError(t, err)

		require.True(t, transaction.AssertCalled(
			t,
			"ExecuteSmartContract",
			context.Background(),
			wallet.ZCNSCSmartContractAddress,
			wallet.BurnFunc,
			zcnsc.BurnPayload{
				EthereumAddress: ethereumAddress,
			},
			uint64(amount),
		))
	})

	t.Run("should check configuration used by AddEthereumAuthorizer", func(t *testing.T) {
		_, err := bridgeClient.AddEthereumAuthorizer(context.Background(), common.HexToAddress(authorizerDelegatedAddress))
		require.NoError(t, err)

		to := common.HexToAddress(bridgeAddress)
		fromAddress := common.HexToAddress(ethereumAddress)

		abi, err := authorizers.AuthorizersMetaData.GetAbi()
		require.NoError(t, err)

		pack, err := abi.Pack("addAuthorizers", common.HexToAddress(authorizerDelegatedAddress))
		require.NoError(t, err)

		require.True(t, ethereumClient.AssertCalled(
			t,
			"EstimateGas",
			context.Background(),
			eth.CallMsg{
				To:   &to,
				From: fromAddress,
				Data: pack,
			},
		))
	})

	t.Run("should check configuration used by RemoveAuthorizer", func(t *testing.T) {
		_, err := bridgeClient.RemoveEthereumAuthorizer(context.Background(), common.HexToAddress(authorizerDelegatedAddress))
		require.NoError(t, err)

		to := common.HexToAddress(bridgeAddress)
		fromAddress := common.HexToAddress(ethereumAddress)

		abi, err := authorizers.AuthorizersMetaData.GetAbi()
		require.NoError(t, err)

		pack, err := abi.Pack("removeAuthorizers", common.HexToAddress(authorizerDelegatedAddress))
		require.NoError(t, err)

		require.True(t, ethereumClient.AssertCalled(
			t,
			"EstimateGas",
			context.Background(),
			eth.CallMsg{
				To:   &to,
				From: fromAddress,
				Data: pack,
			},
		))
	})

	t.Run("should check configuration used by IncreaseBurnerAllowance", func(t *testing.T) {
		_, err := bridgeClient.IncreaseBurnerAllowance(context.Background(), amount)
		require.NoError(t, err)

		spenderAddress := common.HexToAddress(bridgeAddress)

		to := common.HexToAddress(tokenAddress)
		fromAddress := common.HexToAddress(ethereumAddress)

		abi, err := erc20.ERC20MetaData.GetAbi()
		require.NoError(t, err)

		pack, err := abi.Pack("increaseAllowance", spenderAddress, big.NewInt(amount))
		require.NoError(t, err)

		require.True(t, ethereumClient.AssertCalled(
			t,
			"EstimateGas",
			context.Background(),
			eth.CallMsg{
				To:   &to,
				From: fromAddress,
				Data: pack,
			},
		))
	})

	t.Run("should check configuration used by CreateSignedTransactionFromKeyStore", func(t *testing.T) {
		bridgeClient.CreateSignedTransactionFromKeyStore(ethereumClient, 400000)

		require.True(t, ethereumClient.AssertCalled(
			t,
			"PendingNonceAt",
			context.Background(),
			common.HexToAddress(ethereumAddress)))

		require.True(t, keyStore.AssertCalled(
			t,
			"TimedUnlock",
			accounts.Account{
				Address: common.HexToAddress(ethereumAddress),
			},
			password,
			time.Second*2,
		))
	})
}
