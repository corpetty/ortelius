package services

import (
	"github.com/corpetty/avalanchego/genesis"
	"github.com/corpetty/avalanchego/ids"
	avmVM "github.com/corpetty/avalanchego/vms/avm"
	"github.com/corpetty/avalanchego/vms/platformvm"
)

type GenesisContainer struct {
	NetworkID       uint32
	XChainGenesisTx *platformvm.Tx
	XChainID        ids.ID
	AvaxAssetID     ids.ID
	GenesisBytes    []byte
}

func NewGenesisContainer(networkID uint32) (*GenesisContainer, error) {
	gc := &GenesisContainer{NetworkID: networkID}
	var err error
	gc.GenesisBytes, gc.AvaxAssetID, err = genesis.Genesis(gc.NetworkID, "")
	if err != nil {
		return nil, err
	}

	gc.XChainGenesisTx, err = genesis.VMGenesis(gc.GenesisBytes, avmVM.ID)
	if err != nil {
		return nil, err
	}

	gc.XChainID = gc.XChainGenesisTx.ID()
	return gc, nil
}
