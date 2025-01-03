// Copyright 2021 Evmos Foundation
// This file is part of Evmos' Ethermint library.
//
// The Ethermint library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The Ethermint library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the Ethermint library. If not, see https://github.com/evmos/ethermint/blob/main/LICENSE
package evm

import (
	"bytes"
	"fmt"
	abci "github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/evmos/ethermint/utils"
	"strings"

	ethermint "github.com/evmos/ethermint/types"
	"github.com/evmos/ethermint/x/evm/keeper"
	"github.com/evmos/ethermint/x/evm/types"
)

// InitGenesis initializes genesis state based on exported genesis
func InitGenesis(
	ctx sdk.Context,
	k *keeper.Keeper,
	accountKeeper types.AccountKeeper,
	bankKeeper types.BankKeeper,
	data types.GenesisState,
) []abci.ValidatorUpdate {
	if utils.IsOneOfDymensionChains(ctx) && data.Params.EnableCreate {
		panic(fmt.Errorf("enable create is not allowed on Dymension chains"))
	}

	k.WithChainID(ctx)

	err := k.SetParams(ctx, data.Params)
	if err != nil {
		panic(fmt.Errorf("error setting params %s", err))
	}

	// ensure evm module accounts are set
	if addr := accountKeeper.GetModuleAddress(types.ModuleName); addr == nil {
		panic("the EVM module account has not been set")
	}
	if addr := accountKeeper.GetModuleAddress(types.ModuleVirtualFrontierContractDeployerName); addr == nil {
		panic("the VFC deployer account has not been set")
	}

	for _, account := range data.Accounts {
		address := common.HexToAddress(account.Address)
		accAddress := sdk.AccAddress(address.Bytes())
		// check that the EVM balance the matches the account balance
		acc := accountKeeper.GetAccount(ctx, accAddress)
		if acc == nil {
			panic(fmt.Errorf("account not found for address %s", account.Address))
		}

		ethAcct, ok := acc.(ethermint.EthAccountI)
		if !ok {
			panic(
				fmt.Errorf("account %s must be an EthAccount interface, got %T",
					account.Address, acc,
				),
			)
		}
		code := common.Hex2Bytes(account.Code)
		codeHash := crypto.Keccak256Hash(code)

		// we ignore the empty Code hash checking, see ethermint PR#1234
		if len(account.Code) != 0 && !bytes.Equal(ethAcct.GetCodeHash().Bytes(), codeHash.Bytes()) {
			s := "the evm state code doesn't match with the codehash\n"
			panic(fmt.Sprintf("%s account: %s , evm state codehash: %v, ethAccount codehash: %v, evm state code: %s\n",
				s, account.Address, codeHash, ethAcct.GetCodeHash(), account.Code))
		}

		k.SetCode(ctx, codeHash.Bytes(), code)

		for _, storage := range account.Storage {
			k.SetState(ctx, address, common.HexToHash(storage.Key), common.HexToHash(storage.Value).Bytes())
		}
	}

	if utils.IsEthermintDevChain(ctx) {
		// devnet

		denom := data.Params.EvmDenom

		_, found := k.GetVirtualFrontierBankContractAddressByDenom(ctx, denom)
		if !found {
			vfBankContractOfNativeMeta := types.VFBankContractMetadata{
				MinDenom: denom,
			}

			var bankDenomMetadataOfNative banktypes.Metadata

			bankDenomMetadataOfNative, found := bankKeeper.GetDenomMetaData(ctx, denom)
			if !found {
				// if the metadata is not found, we create a new one
				// and set it to bank denom metadata store upon genesis initialization
				name := strings.ToUpper(denom[1:])
				bankDenomMetadataOfNative = banktypes.Metadata{
					DenomUnits: []*banktypes.DenomUnit{
						{
							Denom:    denom,
							Exponent: 0,
						},
						{
							Denom:    name,
							Exponent: 18,
						},
					},
					Base:    denom,
					Display: name,
					Name:    name,
					Symbol:  name,
				}
				bankKeeper.SetDenomMetaData(ctx, bankDenomMetadataOfNative)
			}

			denomMetadata, valid := types.CollectMetadataForVirtualFrontierBankContract(bankDenomMetadataOfNative)
			if !valid {
				panic(fmt.Sprintf("prepared bank denom metadata for native assets is invalid: %v", bankDenomMetadataOfNative))
			}

			contract, err := k.DeployNewVirtualFrontierBankContract(ctx,
				&types.VirtualFrontierContract{
					Active: true,
				}, &vfBankContractOfNativeMeta,
				&denomMetadata,
			)
			if err != nil {
				panic(err)
			}

			ctx.Logger().Info("deployed virtual frontier bank contract for native token on devnet", "address", contract.String())
		}
	}

	if len(data.VirtualFrontierContracts) > 0 {
		if err := k.GenesisImportVirtualFrontierContracts(ctx, data.VirtualFrontierContracts); err != nil {
			panic(err)
		}
	}

	{ // ensure deployer sequence number matches number of deployed VFCs
		var deployedVFCsCount uint64
		k.IterateVirtualFrontierContracts(ctx, func(_ types.VirtualFrontierContract) bool {
			deployedVFCsCount++
			return false
		})
		deployerModuleAccount := accountKeeper.GetModuleAccount(ctx, types.ModuleVirtualFrontierContractDeployerName)
		if deployerModuleAccount.GetSequence() != deployedVFCsCount {
			panic(fmt.Errorf("invalid sequence number for deployer account, expect sequence %d but got %d", deployedVFCsCount, deployerModuleAccount.GetSequence()))
		}
	}

	return []abci.ValidatorUpdate{}
}

// ExportGenesis exports genesis state of the EVM module
func ExportGenesis(ctx sdk.Context, k *keeper.Keeper, ak types.AccountKeeper) *types.GenesisState {
	var ethGenAccounts []types.GenesisAccount
	ak.IterateAccounts(ctx, func(account authtypes.AccountI) bool {
		ethAccount, ok := account.(ethermint.EthAccountI)
		if !ok {
			// ignore non EthAccounts
			return false
		}

		addr := ethAccount.EthAddress()

		storage := k.GetAccountStorage(ctx, addr)

		genAccount := types.GenesisAccount{
			Address: addr.String(),
			Code:    common.Bytes2Hex(k.GetCode(ctx, ethAccount.GetCodeHash())),
			Storage: storage,
		}

		ethGenAccounts = append(ethGenAccounts, genAccount)
		return false
	})

	var vfContracts []types.VirtualFrontierContract
	k.IterateVirtualFrontierContracts(ctx, func(contract types.VirtualFrontierContract) bool {
		vfContracts = append(vfContracts, contract)
		return false
	})

	return &types.GenesisState{
		Accounts:                 ethGenAccounts,
		Params:                   k.GetParams(ctx),
		VirtualFrontierContracts: vfContracts,
	}
}
