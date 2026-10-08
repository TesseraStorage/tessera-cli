package accounts

import (
	"errors"
	"time"

	proto "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
)

var (
	// ErrExists is returned by database operations that fail due to an account
	// already existing.
	ErrExists = errors.New("account already exists")

	// ErrNotFound is returned by database operations that fail due to an
	// account not being found.
	ErrNotFound = errors.New("account not found")

	// ErrNoQuota is returned when trying to create a connect key without a quota.
	ErrNoQuota = errors.New("quota is required")

	// ErrAccountStorageLimitExceeded is returned when an operation fails due
	// to the account exceeding its storage limit.  We use the term "app
	// storage limit" here because from the user's perspective, they will have
	// one connect key with multiple apps attached, each of which is
	// actually represented by an account in the database.
	ErrAccountStorageLimitExceeded = errors.New("app storage limit exceeded")

	// ErrCreditIDReused is returned when a funding budget credit id has
	// already been applied to a different account.
	ErrCreditIDReused = errors.New("credit id already used")

	// ErrAccountBudgetExhausted is returned when a new pin would exceed the
	// account's funding budget, or when the account is under the
	// plan-pending quota.
	ErrAccountBudgetExhausted = errors.New("funding budget exhausted")

	// ErrFundingRateUnavailable is returned when a budget-enforced pin cannot
	// be charged because no fresh SC/USD rate or no good-for-append contract
	// is available to price it.
	ErrFundingRateUnavailable = errors.New("funding rate unavailable")

	// ErrInvalidDelta is returned when a funding budget increment is
	// negative.
	ErrInvalidDelta = errors.New("funding budget increment must be non-negative")
)

type (
	// AddAccountOptions holds optional parameters for account creation.
	AddAccountOptions struct {
		MaxPinnedData uint64
	}

	// AddAccountOption is a functional option for configuring optional
	// parameters during account creation.
	AddAccountOption func(*AddAccountOptions)
)

// WithMaxPinnedData sets the maximum amount of data that can be pinned
func WithMaxPinnedData(maxPinnedData uint64) AddAccountOption {
	return func(opts *AddAccountOptions) {
		opts.MaxPinnedData = maxPinnedData
	}
}

type (
	// QueryAccountsOptions holds options for querying accounts.
	QueryAccountsOptions struct {
		ConnectKey *string
	}

	// QueryAccountsOpt is a functional option for querying accounts.
	QueryAccountsOpt func(o *QueryAccountsOptions)
)

// WithConnectKey filters the accounts by the connect key they are associated
// with.
func WithConnectKey(connectKey string) QueryAccountsOpt {
	return func(opt *QueryAccountsOptions) {
		opt.ConnectKey = &connectKey
	}
}

type (
	// Account represents an account in the indexer.
	Account struct {
		AccountKey           proto.Account `json:"accountKey"`
		ConnectKey           string        `json:"connectKey"`
		MaxPinnedData        uint64        `json:"maxPinnedData"`
		QuotaMaxPinnedData   uint64        `json:"quotaMaxPinnedData"`
		ConnectKeyPinnedData uint64        `json:"connectKeyPinnedData"`
		Ready                bool          `json:"ready"`
		PinnedData           uint64        `json:"pinnedData"`
		PinnedSize           uint64        `json:"pinnedSize"`
		App                  AppMeta       `json:"app"`
		LastUsed             time.Time     `json:"lastUsed"`
	}

	// HostAccount represents an ephemeral account on a host.
	HostAccount struct {
		AccountKey             proto.Account
		HostKey                types.PublicKey
		ConsecutiveFailedFunds int
		NextFund               time.Time
		FullStorage            bool
		// Enforced is true when the account's funding is budget-enforced
		// (non-NULL funding budget or the plan-pending quota). Legacy
		// candidates report false. Only funding candidates set this.
		Enforced bool
		// Existing is true when the candidate came from an account_hosts row
		// (the account was already funded on this host at least once). New
		// candidates report false. Used to avoid marking a never-funded
		// account ready when its funding is deferred (e.g. no SC/USD rate).
		Existing bool
	}

	// FundingBudget describes an account's USD funding budget and spend. A
	// NULL budget (Enforced false) means the account is unenforced and keeps
	// the legacy funding behaviour.
	FundingBudget struct {
		BudgetUSD float64 `json:"fundingBudgetUSD"`
		SpentUSD  float64 `json:"fundingSpentUSD"`
		// Enforced is not serialized: the admin endpoint's response body
		// carries only the two USD amounts.
		Enforced bool `json:"-"`
	}

	// HostPool represents a balance pool on a host.
	HostPool struct {
		HostKey                types.PublicKey
		PoolKey                types.PrivateKey
		ConsecutiveFailedFunds int
		NextFund               time.Time
		FullStorage            bool
		// Enforced is true when at least one fundable account on the pool's
		// connect key is budget-enforced (non-NULL funding budget or the
		// plan-pending quota). Legacy pools report false. Only funding
		// candidates set this.
		Enforced bool
		// Existing is true when the candidate came from a pool_hosts row (the
		// pool was already funded on this host at least once). New candidates
		// report false. Used to avoid marking a never-funded pool ready when
		// its funding is deferred (e.g. no SC/USD rate).
		Existing bool
	}

	// PendingAttachment represents an account that needs to be attached to a
	// pool on a specific host.
	PendingAttachment struct {
		AccountKey proto.Account
		PoolKey    types.PrivateKey
	}

	// UpdateAccountRequest is the request body for the
	// [PUT] /account/:accountkey/limit endpoint.
	UpdateAccountRequest struct {
		MaxPinnedData *uint64 `json:"maxPinnedData"`
	}

	// QuotaFundInfo contains funding info for a quota. Active is the number
	// of active funding units and FullStorage is the subset of those that
	// have reached their storage limit.
	QuotaFundInfo struct {
		QuotaName       string
		FundTargetBytes uint64
		Active          uint64
		FullStorage     uint64
	}
)
