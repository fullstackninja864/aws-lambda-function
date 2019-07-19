package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssm"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts/chequebook"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"gitlab.com/deqode/tokenholder-management-lambda/blockchain/contracts"
	"gitlab.com/deqode/tokenholder-management-lambda/config"
	"gitlab.com/deqode/tokenholder-management-lambda/types"
)

var (
	int2     = big.NewInt(2)
	int3     = big.NewInt(3)
	gasLimit = int64(21000)
)

func init() {
	path, found := os.LookupEnv("SSM_PS_PATH")
	if found {
		session, err := session.NewSession(aws.NewConfig())
		if err != nil {
			log.Printf("error: failed to create new session %s\n", err)
			os.Exit(1)
		}
		service := ssm.New(session)
		withDecryption := true
		request := ssm.GetParametersByPathInput{Path: &path, WithDecryption: &withDecryption}
		response, err := service.GetParametersByPath(&request)
		if err != nil {
			log.Printf("error: failed to get parameters, %s\n", err)
			os.Exit(1)
		}
		for _, parameter := range response.Parameters {
			paramName := strings.ToUpper(strings.TrimPrefix(*parameter.Name, path))
			os.Setenv(paramName, *parameter.Value)
			log.Printf("set env variable: %s\n", paramName)
		}
	}
}

// HandleEvent will handle the event that triggered Lambda function
func HandleEvent(ctx context.Context, event events.CloudWatchEvent) (string, error) {
	err := config.Parse()
	if err != nil {
		return formatError("Unable to parse environment variables with error : %s", err)
	}

	// Update the ethereum json RPC url as per the enviroment
	ethClient, err := ethclient.Dial(config.EthereumJSONRPCURL)
	if err != nil {
		return formatError("Failed to call ethereum client with error : %s", err)
	}

	gasPrice, err := ethClient.SuggestGasPrice(ctx)
	if err != nil {
		return formatError("Failed to get gas price from ethereum client with error : %s", err)
	}

	// deqode vault private key
	deqodeVaultPrivateKey, err := crypto.HexToECDSA(config.deqodeVaultPrivateKey)
	if err != nil {
		return formatError("Failed to parse ecdsa private key from deqode vault private key sting with error : %s", err)
	}

	// token holder processing key
	tokenHolderProcessingKey, err := crypto.HexToECDSA(config.TokenHolderProcessingKey)
	if err != nil {
		return formatError("Failed to parse ecdsa private key from token holder processing key string with error : %s", err)
	}

	// deqode address (to which 2/3rd of revenue will be transfered)
	deqodeAddress := common.HexToAddress(config.deqodeAddress)

	// Token holder contract address
	tokenHolderContractAddress := common.HexToAddress(config.TokenHolderContractAddress)

	// deqode vault address
	deqodeVaultAddress := crypto.PubkeyToAddress(deqodeVaultPrivateKey.PublicKey)
	vaultBalance, err := ethClient.BalanceAt(ctx, deqodeVaultAddress, nil)
	if err != nil {
		return formatError("Failed to get balance for vault with error : %s", err)
	}

	// Some balance is left to cover txns fees (gaslimit* 2 * gasPrice)
	vaultBalance.Sub(vaultBalance, new(big.Int).Mul(big.NewInt(gasLimit*2), gasPrice))

	// Check if balance is positive after fees is subtracted
	if vaultBalance.Cmp(big.NewInt(0)) != 1 {
		return formatError("Not enough balance in vault to cover txn fees")
	}

	// Set auth key, gasPrice, gasLimit
	vaultAuth := bind.NewKeyedTransactor(deqodeVaultPrivateKey)
	vaultAuth.GasPrice = gasPrice
	vaultAuth.GasLimit = uint64(gasLimit)

	// Transferfing 1/3 vault balance to token holder contract
	vaultAuth.Value = new(big.Int).Div(vaultBalance, int3)
	txn, err := ethereum.Transferer{ContractTransactor: ethClient}.Transfer(vaultAuth, tokenHolderContractAddress, nil)
	if err != nil {
		return formatError("Failed to transfer vault balance to token holder contract with error : %s", err)
	}

	// waiting for token holder fund transfer txn
	err = waitForTx(ctx, ethClient, txn.Hash())
	if err != nil {
		return formatError("Fund transfer txn with hash %s failed  with error : %s", txn.Hash().Hex(), err)
	}

	// Transferfing 2/3 vault balance to deqode address
	vaultAuth.Value = new(big.Int).Mul(new(big.Int).Div(vaultBalance, int3), int2)
	txn, err = ethereum.Transferer{ContractTransactor: ethClient}.Transfer(vaultAuth, deqodeAddress, nil)
	if err != nil {
		return formatError("Failed to transfer vault balance to deqode address with error : %s", err)
	}

	// waiting for deqode address fund transfer txn
	err = waitForTx(ctx, ethClient, txn.Hash())
	if err != nil {
		return formatError("Fund transfer txn with hash %s failed  with error : %s", txn.Hash().Hex(), err)
	}

	// Creating instance for token holder program contract
	tokenHolderContract, err := contracts.NewdeqodeTokenHolderProgramContract(tokenHolderContractAddress, ethClient)
	if err != nil {
		return formatError("Failed to load token holder contract for address %s with error : %s", tokenHolderContractAddress.String(), err)
	}

	tokenHolderAuth := bind.NewKeyedTransactor(tokenHolderProcessingKey)
	// Calling sell vouchers of token holder program contract
	txn, err = tokenHolderContract.SellVouchers(tokenHolderAuth)
	if err != nil {
		return formatError("Failed to  call sell vouchers of token holder program contract with error : %s", err)
	}

	// waiting for token holder sell voucher txn
	err = waitForTx(ctx, ethClient, txn.Hash())
	if err != nil {
		return formatError("Sell vouchers txn with hash %s failed  with error : %s", txn.Hash().Hex(), err)
	}

	// Calling buy vouchers of token holder program contract
	txn, err = tokenHolderContract.BuyVouchers(tokenHolderAuth)
	if err != nil {
		return formatError("Failed to  call buy vouchers of token holder program contract with error : %s", err)
	}

	// waiting for token holder buy voucher txn
	err = waitForTx(ctx, ethClient, txn.Hash())
	if err != nil {
		return formatError("Buy vouchers txn with hash %s failed  with error : %s", txn.Hash().Hex(), err)
	}

	return fmt.Sprintf("Token holder event successfully called at %s!", event.Time), nil
}

func main() {
	lambda.Start(HandleEvent)
}

func waitForTx(ctx context.Context, backend chequebook.Backend, txHash common.Hash) error {
	log.Printf("Waiting for transaction: 0x%x", txHash)

	type commiter interface {
		Commit()
	}
	if sim, ok := backend.(commiter); ok {
		sim.Commit()
		tr, err := backend.TransactionReceipt(ctx, txHash)
		if err != nil {
			return err
		}
		if tr.Status != types.ReceiptStatusSuccessful {
			return fmt.Errorf("tx failed: %+v", tr)
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(4 * time.Second):
		}

		tr, err := backend.TransactionReceipt(ctx, txHash)
		if err != nil {
			if err == ethereum.NotFound {
				continue
			} else {
				return err
			}
		} else {
			if tr.Status != types.ReceiptStatusSuccessful {
				return fmt.Errorf("tx failed: %+v", tr)
			}
			return nil
		}
	}
}

func formatError(format string, a ...interface{}) (string, error) {
	return "", fmt.Errorf(format, a...)
}
