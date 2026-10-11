// Package contracttypes holds the contract data types shared by the funding
// package and by packages that must reason about contracts without linking the
// funding code (notably slabs). The contracts package aliases these types, so
// they remain wire- and API-compatible.
package contracttypes

import (
	"fmt"
	"time"

	proto "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils/rhp/v4"
)

// The following consts define the various states a contract can be in.
const (
	ContractStatePending = ContractState(iota)
	ContractStateActive
	ContractStateResolved
	ContractStateExpired
	ContractStateRejected
)

// MaxContractSize is the maximum size of a contract in bytes.
const MaxContractSize = 10 * 1 << 40 // 10TB

const (
	// MinContractGrowthRate is the minimum expected growth rate for contracts
	// used when calculating funding. Lowering this value will mean contracts
	// will need to be refreshed more frequently. 32 GiB is a good trade off
	// between initial cost to both parties and the frequency of refreshes.
	MinContractGrowthRate = 32 << 30

	// MaxContractGrowthRate is the maximum additional data allowed when adding
	// funds for refresh or renews. This means contracts will not grow
	// exponentially as more data is uploaded. Decreasing this will mean
	// contracts will need to be refreshed more frequently. Increasing this
	// will mean large contracts will be more expensive. 256 GiB is a good
	// trade off between cost and frequency of refreshes due to how long it
	// would take to reasonably upload that amount of data with a 10 Gbps
	// connection.
	MaxContractGrowthRate = 256 << 30
)

// MinHostCollateral is the minimum collateral the renter will request when
// forming, refreshing, or renewing a contract.
var MinHostCollateral = types.Siacoins(1)

type (
	// ContractState describes the current state of the contract on the network
	// - pending: the contract has not yet been seen on-chain
	// - active: the contract was mined on-chain
	// - resolved: the contract was renewed or a valid storage proof has been submitted
	// - expired: the contract has expired without a valid storage proof
	// - rejected: the contract didn't make it into a block
	ContractState uint

	// ContractSpending describes the spending of a contract. Every time
	// contract funds are moved from the indexer to a host, the spending is
	// recorded.
	ContractSpending struct {
		AppendSector types.Currency `json:"appendSector"`
		FreeSector   types.Currency `json:"freeSector"`
		FundAccount  types.Currency `json:"fundAccount"`
		SectorRoots  types.Currency `json:"sectorRoots"`
	}

	// Contract is a contract formed with a host
	Contract struct {
		// static fields
		ID          types.FileContractID `json:"id"`
		HostKey     types.PublicKey      `json:"hostKey"`
		Formation   time.Time            `json:"formation"`
		RenewedFrom types.FileContractID `json:"renewedFrom"`

		// event tracking fields
		NextPrune            time.Time `json:"nextPrune"`
		LastBroadcastAttempt time.Time `json:"lastBroadcastAttempt"`

		// revision fields, updated on refresh/renewal
		RevisionNumber     uint64         `json:"revisionNumber"`     // current revision number
		ProofHeight        uint64         `json:"proofHeight"`        // start of the contract's proof window
		ExpirationHeight   uint64         `json:"expirationHeight"`   // end of the contract's proof window
		Capacity           uint64         `json:"capacity"`           // already paid for capacity (always >=Size)
		Size               uint64         `json:"size"`               // current size of the contract
		InitialAllowance   types.Currency `json:"initialAllowance"`   // initial renter allowance locked in contract
		RemainingAllowance types.Currency `json:"remainingAllowance"` // remaining renter allowance
		TotalCollateral    types.Currency `json:"totalCollateral"`    // total amount of collateral locked in contract

		// updated on refresh/renewal
		RenewedTo      types.FileContractID `json:"renewedTo"`
		UsedCollateral types.Currency       `json:"usedCollateral"`
		ContractPrice  types.Currency       `json:"contractPrice"` // price of the contract creation as charged by the host
		MinerFee       types.Currency       `json:"minerFee"`      // miner fee spent on formation txn

		// state fields, reset on refresh/renewal
		//
		// Good determines whether a contract is good or bad. A contract that
		// is not good, will have its data migrated to a new contract.
		//
		// A contract can be bad for multiple reasons such as the host being
		// considered bad or failing to renew when being too close to its
		// ProofHeight. This field is set by the contract maintenance code.
		Good     bool             `json:"good"`
		State    ContractState    `json:"state"`
		Spending ContractSpending `json:"spending"`
	}
)

// GoodForAccountFunding indicates whether a contract has enough remaining
// allowance to spend the target amount.
func (c Contract) GoodForAccountFunding(target types.Currency) error {
	switch {
	case !c.Good:
		return fmt.Errorf("contract is not good")
	case c.RemainingAllowance.Cmp(target) < 0:
		return fmt.Errorf("not enough remaining allowance to fund accounts, need %s, have %s", target, c.RemainingAllowance)
	default:
		return nil
	}
}

func (c Contract) inRenewWindow(renewWindow, height uint64) bool {
	return c.ProofHeight <= height+renewWindow
}

// GoodForAppend indicates whether a contract can be used to append sectors
func (c Contract) GoodForAppend(settings proto.HostSettings, renewWindow, height, period uint64) error {
	prices := settings.Prices
	maxRenewableSize := MaxRenewableContractSize(settings, period)
	switch {
	case !c.Good:
		return fmt.Errorf("contract is not good")
	case c.Size >= maxRenewableSize:
		return fmt.Errorf("contract has reached maximum renewable size: %d >= %d", c.Size, maxRenewableSize)
	case c.ProofHeight <= height:
		return fmt.Errorf("contract is not revisable")
	case c.inRenewWindow(renewWindow, height):
		return fmt.Errorf("contract is in renew window")
	case c.Size < c.Capacity:
		return nil
	}
	// check if there is enough allowance and collateral to append at least one sector
	appendUsage := prices.RPCAppendSectorsCost(1, c.ExpirationHeight-height)
	renterCost, hostCollateral := appendUsage.RenterCost(), appendUsage.HostRiskedCollateral()
	remainingCollateral := c.TotalCollateral.Sub(c.UsedCollateral)
	if c.RemainingAllowance.Cmp(renterCost) < 0 {
		return fmt.Errorf("not enough remaining allowance to append a sector, need %s, have %s", renterCost, c.RemainingAllowance)
	} else if remainingCollateral.Cmp(hostCollateral) < 0 {
		return fmt.Errorf("not enough remaining collateral to append a sector, need %s, have %s", hostCollateral, remainingCollateral)
	}
	return nil
}

// GoodForRefresh indicates whether a contract is likely to succeed refreshing.
func (c Contract) GoodForRefresh(settings proto.HostSettings, fundTarget types.Currency, renewWindow, height, period uint64) error {
	if c.inRenewWindow(renewWindow, height) {
		return fmt.Errorf("contract is in renew window")
	}

	duration := c.ExpirationHeight - height
	_, collateral := ContractFunding(settings, c.Size, fundTarget, duration)
	var totalCollateral types.Currency
	if settings.ProtocolVersion.Cmp(rhp.ProtocolVersion502) >= 0 {
		totalCollateral = c.UsedCollateral.Add(collateral)
	} else {
		totalCollateral = c.TotalCollateral.Add(collateral)
	}
	maxRenewableSize := MaxRenewableContractSize(settings, period)
	switch {
	case !c.Good:
		return fmt.Errorf("contract is not good")
	case c.Size >= maxRenewableSize:
		return fmt.Errorf("contract has reached maximum renewable size: %d >= %d", c.Size, maxRenewableSize)
	case c.ProofHeight <= height:
		return fmt.Errorf("contract is not revisable")
	case totalCollateral.Cmp(settings.MaxCollateral) > 0:
		return fmt.Errorf("host's max collateral %s is less than estimated collateral %s for refresh", settings.MaxCollateral, totalCollateral)
	default:
		return nil
	}
}

// String implements the fmt.Stringer interface.
func (s ContractState) String() string {
	switch s {
	case ContractStatePending:
		return "pending"
	case ContractStateActive:
		return "active"
	case ContractStateResolved:
		return "resolved"
	case ContractStateExpired:
		return "expired"
	case ContractStateRejected:
		return "rejected"
	default:
		return ""
	}
}

// MarshalText implements the encoding.TextMarshaler interface.
func (s ContractState) MarshalText() ([]byte, error) {
	if str := s.String(); str == "" {
		return nil, fmt.Errorf("unknown contract state %v", s)
	} else {
		return []byte(str), nil
	}
}

// UnmarshalText implements the encoding.TextUnmarshaler interface.
func (s *ContractState) UnmarshalText(b []byte) error {
	switch string(b) {
	case "pending":
		*s = ContractStatePending
		return nil
	case "active":
		*s = ContractStateActive
		return nil
	case "resolved":
		*s = ContractStateResolved
		return nil
	case "expired":
		*s = ContractStateExpired
		return nil
	case "rejected":
		*s = ContractStateRejected
		return nil
	default:
		return fmt.Errorf("unknown contract state %v", s)
	}
}

// MaxRenewableContractSize returns the maximum size a contract can have to
// still be renewable.
func MaxRenewableContractSize(hostSettings proto.HostSettings, period uint64) uint64 {
	maxCollateral := hostSettings.MaxCollateral
	sectorUsage := hostSettings.Prices.RPCAppendSectorsCost(1, period+proto.ProofWindow)
	sectorCollateral := sectorUsage.HostRiskedCollateral()
	if sectorCollateral.IsZero() {
		sectorCollateral = types.NewCurrency64(1)
	}
	maxSectors := maxCollateral.Div(sectorCollateral)
	maxSectorsSize := maxSectors.Mul64(proto.SectorSize).Big()
	maxSize := uint64(MaxContractSize)
	if maxSectorsSize.IsUint64() {
		maxSize = min(maxSize, maxSectorsSize.Uint64())
	}

	// 20% safety margin
	maxSize = maxSize * 8 / 10

	// round down to a multiple of the sector size
	maxSize = (maxSize / proto.SectorSize) * proto.SectorSize
	return maxSize
}

// ContractFunding calculates the funding and collateral that go into forming,
// refreshing or renewing a contract.
func ContractFunding(settings proto.HostSettings, existingData uint64, minAllowance types.Currency, duration uint64) (allowance, collateral types.Currency) {
	multiplier := 1 + (existingData / MinContractGrowthRate)
	contractGrowth := min(MinContractGrowthRate*multiplier, MaxContractGrowthRate) / proto.SectorSize // 100% growth clamped to [32GiB, 256GiB]
	uploadCost := settings.Prices.RPCWriteSectorCost(proto.SectorSize).RenterCost().Mul64(contractGrowth)
	downloadCost := settings.Prices.RPCReadSectorCost(proto.SectorSize).RenterCost().Mul64(contractGrowth)
	storeCost := settings.Prices.RPCAppendSectorsCost(contractGrowth, duration).RenterCost()
	allowance = uploadCost.Add(storeCost).Add(downloadCost)
	if allowance.Cmp(minAllowance) < 0 {
		allowance = minAllowance // ensure we have at least the minimum allowance
	}

	collateral = proto.MaxHostCollateral(settings.Prices, storeCost) // based on store cost because uploads do not require collateral
	if collateral.Cmp(settings.MaxCollateral) > 0 {
		collateral = settings.MaxCollateral
	}
	if collateral.Cmp(MinHostCollateral) < 0 {
		collateral = MinHostCollateral // ensure we have at least the minimum collateral
	}
	return
}
