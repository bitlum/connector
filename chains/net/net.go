package net

import (
	"github.com/bitlum/btcd/chaincfg"
	"github.com/bitlum/connector/chains/bitcoincash"
	"github.com/bitlum/connector/chains/dash"
	"github.com/bitlum/connector/chains/litecoin"
	"github.com/go-errors/errors"
)

func GetParams(asset string, netName string) (*chaincfg.Params, error) {
	switch asset {
	case "BTC":
		switch netName {
		case "mainnet":
			return &chaincfg.MainNetParams, nil
		case "regtest":
			return &chaincfg.RegressionNetParams, nil
		case "testnet3":
			return &chaincfg.TestNet3Params, nil
		default:
			return nil, errors.New("invalid or unsupported net")
		}
	case "BCH":
		switch netName {
		case "mainnet":
			return &bitcoincash.MainNetParams, nil
		case "regtest":
			return &bitcoincash.RegressionNetParams, nil
		case "testnet3":
			return &bitcoincash.TestNet3Params, nil
		}
	case "LTC":
		switch netName {
		case "mainnet":
			return &litecoin.MainNetParams, nil
		case "mainnet-legacy":
			return &litecoin.MainNetParamsLegacy, nil
		case "regtest":
			return &litecoin.RegressionNetParams, nil
		case "testnet4":
			return &litecoin.TestNet4Params, nil
		}
	case "DASH":
		switch netName {
		case "mainnet":
			return &dash.MainNetParams, nil
		case "regtest":
			return &dash.RegressionNetParams, nil
		case "testnet3":
			return &dash.TestNet3Params, nil
		}
	default:
		return nil, errors.Errorf("%s asset is invalid or unsupported", asset)
	}
	return nil, errors.Errorf("%s asset's net %s is invalid or unsupported", asset, netName)
}
