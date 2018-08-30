package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
)

// taken from github.com/sonm-io/core/proto/marketplace.pb.go
// IdentityLevel type changed to uint8 because that's what
// GetProfileLevel of marketplace contract returns
type IdentityLevel uint8

const (
	IdentityLevel_UNKNOWN      IdentityLevel = 0
	IdentityLevel_ANONYMOUS    IdentityLevel = 1
	IdentityLevel_REGISTERED   IdentityLevel = 2
	IdentityLevel_IDENTIFIED   IdentityLevel = 3
	IdentityLevel_PROFESSIONAL IdentityLevel = 4
)

const (
	updateInterval = 5 * time.Second
)

var accountSlotsDefaults = map[IdentityLevel]uint64{
	IdentityLevel_UNKNOWN: 16,
	IdentityLevel_ANONYMOUS: 32,
	IdentityLevel_REGISTERED: 64,
	IdentityLevel_IDENTIFIED: 128,
	IdentityLevel_PROFESSIONAL: 256,
}

var getProfileLevelAbi = `[{
      "constant": true,
      "inputs": [
        {
          "name": "_owner",
          "type": "address"
        }
      ],
      "name": "GetProfileLevel",
      "outputs": [
        {
          "name": "",
          "type": "uint8"
        }
      ],
      "payable": false,
      "stateMutability": "view",
      "type": "function"
    }]`

type SonmConfig struct {
	ProfileRegistryAddr string
}

var DefaultSonmConfig = SonmConfig{
	ProfileRegistryAddr: "0x1D4DAf3D826683DA8b68d9f5165ae1196Cf97b52",
}

type SonmExtension interface {
	AccountSlots(common.Address) uint64
	ValidateTransaction(tx *types.Transaction, local bool) error
	Stop()
}

type sonmExtension struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex

	accLevels map[common.Address]IdentityLevel

	abi             abi.ABI
	profileRegistry common.Address
	wg              sync.WaitGroup

	client *ethclient.Client
}

func (m *sonmExtension) Stop() {
	m.cancel()
	m.wg.Wait()
	log.Info("SONM Extensions stopped")
}

func (m *sonmExtension) ValidateTransaction(tx *types.Transaction, local bool) error {
	return nil
}

func (m *sonmExtension) AccountSlots(addr common.Address) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var level IdentityLevel

	level, ok := m.accLevels[addr]
	if !ok {
		level, err := m.getAccountProfile(addr)
		if err != nil {
			level = IdentityLevel_UNKNOWN
		}
		m.accLevels[addr] = level
	}

	if slots, ok := accountSlotsDefaults[level]; ok {
		return slots
	}

	return accountSlotsDefaults[IdentityLevel_UNKNOWN]
}

func (m *sonmExtension) getAccountProfile(addr common.Address) (IdentityLevel, error) {
	out, err := m.abi.Pack("GetProfileLevel", addr)
	if err != nil {
		log.Warn("failed to get profile level", "err", err)
		return IdentityLevel_UNKNOWN, err
	}

	reqCtx, reqCtxCancel := context.WithTimeout(m.ctx, 500 * time.Millisecond)
	defer reqCtxCancel()

	msg := ethereum.CallMsg{To: &m.profileRegistry, Data: out}
	result, err := m.client.CallContract(reqCtx, msg, nil)
	if err != nil {
		log.Warn("failed to call contract", "err", err)
		return IdentityLevel_UNKNOWN, err
	}

	var resultLevel uint8
	err = m.abi.Unpack(&resultLevel, "GetProfileLevel", result)
	if err != nil {
		log.Warn("failed to unpack result", "err", err)
		return IdentityLevel_UNKNOWN, err
	}
	log.Info("Unpacked result", "addr", addr.Hex(), "level", resultLevel)

	return IdentityLevel(resultLevel), nil
}

func (m *sonmExtension) updateLevelInfo() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for addr, level := range m.accLevels {
		resLevel, err := m.getAccountProfile(addr)

		if err != nil {
			continue
		}

		if resLevel != level {
			m.accLevels[addr] = resLevel
		}
	}
}

func (m *sonmExtension) loop() {
	defer m.wg.Done()
	update := time.NewTicker(updateInterval)

	for {
		select {
		case <-update.C:
			m.updateLevelInfo()
		case <-m.ctx.Done():
			return
		}
	}
}

func NewSonmExtension(cfg SonmConfig, client *ethclient.Client) (SonmExtension, error) {
	se := &sonmExtension{}
	se.ctx, se.cancel = context.WithCancel(context.Background())

	abi, err := abi.JSON(strings.NewReader(getProfileLevelAbi))

	if err != nil {
		return nil, err
	}

	if _, ok := abi.Methods["GetProfileLevel"]; !ok {
		return nil, errors.New("failed to find GetProfileLevel in parsed ABI")
	}

	se.abi = abi
	se.profileRegistry = common.HexToAddress(cfg.ProfileRegistryAddr)

	se.accLevels = make(map[common.Address]IdentityLevel)
	se.accLevels[common.HexToAddress("0xfe9e8709d3215310075d67e3ed32a380ccf451c8")] = IdentityLevel_UNKNOWN
	se.accLevels[common.HexToAddress("0xB6b5089844F439018635bab88B36cd4705f0d090")] = IdentityLevel_UNKNOWN

	se.client = client

	se.wg.Add(1)
	go se.loop()

	return se, nil
}
