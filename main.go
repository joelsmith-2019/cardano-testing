package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/blockfrost/blockfrost-go"
	common "github.com/igorcrevar/go-cardano-tx/common"
	cardano "github.com/igorcrevar/go-cardano-tx/core"
	"github.com/igorcrevar/go-cardano-tx/sendtx"
)

const (
	blockfrostUrl = "https://cardano-mainnet.blockfrost.io/api/v0" // mainnet url
	blockfrostKey = "mainnetRMDHLaOZxWE56910BUa1GV98jgC7dwMG"      // <--- PUT PRIVATE KEY HERE. See README.md for more info.
	minUtxoValue  = uint64(1_000_000)
)

// This main method contains some simple blockfrost API calls to familiarize myself with the library. It
// simply queries a specific address and the latest block, and prints out some chain information.
func main() {
	// create a blockfrost client
	client := blockfrost.NewAPIClient(
		blockfrost.APIClientOptions{
			ProjectID:   blockfrostKey,
			Server:      blockfrostUrl,
			MaxRoutines: 10,
		},
	)

	// get the API info
	info, err := client.Info(context.TODO())
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("API Info:\n\tUrl: %s\n\tVersion: %s\n", info.Url, info.Version)

	// query specific address
	addr, err := client.Address(context.TODO(), "addr1qxs7p80zrnp0gnc32qcrn38lav86mr0xlqwma4caayesu4mqs7v6uqj9rvm6w0cnpnmy5kljy02tmye93dpca48vsh4quu99g4")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Address: %s - %s\n", addr.Address, addr.Type)

	extended, err := client.AddressExtended(context.TODO(), addr.Address)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Address Extended: %s\n", extended.Address)
	for _, amt := range extended.Amount {
		fmt.Printf("\tStake: %s - %s\n", amt.Quantity, amt.Unit)
	}

	// query latest block
	txs, err := client.BlockLatestTransactions(context.TODO())
	if err != nil {
		log.Fatal(err)
	}

	// loop through txs in the latest block
	for _, tx := range txs {
		txStr := string(tx)
		res, err := client.Transaction(context.TODO(), txStr)
		if err != nil {
			log.Fatal(err)
		}

		fmt.Println("Block Height", res.BlockHeight)

		// Loop through output amount
		for _, output := range res.OutputAmount {
			fmt.Printf("Transaction: %s\n", output)
		}
	}
}

// createTx creates a transaction that sends lovelace to the receiver address
func createTx(
	cardanoCliBinary string,
	txProvider cardano.ITxProvider,
	wallet *cardano.Wallet,
	testNetMagic uint,
	receiverAddr string,
	lovelaceSendAmount uint64,
	potentialFee uint64,
) ([]byte, string, error) {
	enterptiseAddress, err := cardano.NewEnterpriseAddress(
		cardano.TestNetNetwork, wallet.VerificationKey)
	if err != nil {
		return nil, "", err
	}

	senderAddress := enterptiseAddress.String()
	metadata := map[string]interface{}{
		"0": map[string]interface{}{
			"type": "single",
		},
	}

	builder, err := cardano.NewTxBuilder(cardanoCliBinary)
	if err != nil {
		return nil, "", err
	}

	defer builder.Dispose()

	if err := builder.SetProtocolParametersAndTTL(context.Background(), txProvider, 0); err != nil {
		return nil, "", err
	}

	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return nil, "", err
	}

	utxos, err := txProvider.GetUtxos(context.Background(), senderAddress)
	if err != nil {
		return nil, "", err
	}

	inputs, err := sendtx.GetUTXOsForAmounts(
		utxos, map[string]uint64{
			cardano.AdaTokenName: lovelaceSendAmount + potentialFee + minUtxoValue,
		}, 20, 1)
	if err != nil {
		return nil, "", err
	}

	tokens, err := cardano.GetTokensFromSumMap(inputs.Sum)
	if err != nil {
		return nil, "", err
	}

	lovelaceInputsSum := inputs.Sum[cardano.AdaTokenName]
	outputs := []cardano.TxOutput{
		{
			Addr:   receiverAddr,
			Amount: lovelaceSendAmount,
		},
		{
			Addr:   senderAddress,
			Tokens: tokens,
		},
	}

	builder.SetMetaData(metadataBytes).SetTestNetMagic(testNetMagic)
	builder.AddInputs(inputs.Inputs...).AddOutputs(outputs...)

	fee, err := builder.CalculateFee(1)
	if err != nil {
		return nil, "", err
	}

	builder.SetFee(fee)

	builder.UpdateOutputAmount(-1, lovelaceInputsSum-lovelaceSendAmount-fee)

	txRaw, txHash, err := builder.Build()
	if err != nil {
		return nil, "", err
	}

	txSignedRaw, err := builder.SignTx(txRaw, []cardano.ITxSigner{wallet})
	if err != nil {
		return nil, "", err
	}

	return txSignedRaw, txHash, nil
}

// submitTx submits the transaction and waits for it to be included in the blockchain
func submitTx(
	ctx context.Context,
	txProvider cardano.ITxProvider,
	txRaw []byte,
	txHash string,
	addr string,
	tokenName string,
	amountIncrement uint64,
) error {
	utxos, err := txProvider.GetUtxos(ctx, addr)
	if err != nil {
		return err
	}

	if err := txProvider.SubmitTx(context.Background(), txRaw); err != nil {
		return err
	}

	expectedAtLeast := cardano.GetUtxosSum(utxos)[tokenName] + amountIncrement

	fmt.Println("transaction has been submitted. hash =", txHash)

	newBalance, err := common.ExecuteWithRetry(ctx, func(ctx context.Context) (uint64, error) {
		utxos, err := txProvider.GetUtxos(ctx, addr)
		if err != nil {
			return 0, err
		}

		sum := cardano.GetUtxosSum(utxos)

		if sum[tokenName] < expectedAtLeast {
			return 0, common.ErrRetryTryAgain
		}

		return sum[tokenName], nil
	}, common.WithRetryCount(60))
	if err != nil {
		return err
	}

	fmt.Printf("transaction has been included in block. hash = %s, balance = %d\n", txHash, newBalance)

	return nil
}

// sendTransaction wraps the transaction creation and submission process
func sendTransaction(
	senderKey string,
	receiverAddress string,
	amountLovelace uint64,
) error {
	// Initialize Blockfrost provider
	txProvider := cardano.NewTxProviderBlockFrost(
		blockfrostUrl,
		blockfrostKey,
	)
	defer txProvider.Dispose()

	// Create wallet from signing key
	signingKey, err := cardano.GetKeyBytes(senderKey)
	if err != nil {
		return err
	}
	wallet := cardano.NewWallet(signingKey, nil)

	// Create transaction
	txRaw, txHash, err := createTx(
		"cli",
		txProvider,
		wallet,
		2, // testnet magic
		receiverAddress,
		amountLovelace,
		300_000, // potential fee
	)
	if err != nil {
		return err
	}

	// Submit and monitor transaction
	return submitTx(
		context.Background(),
		txProvider,
		txRaw,
		txHash,
		receiverAddress,
		cardano.AdaTokenName,
		amountLovelace,
	)
}
