package contracttypes

// MaintenanceSettings are the settings relevant to contract maintenance.
type MaintenanceSettings struct {
	// Enabled indicates whether contract maintenance is enabled. If false,
	// account funding, pinning and pruning will happen on existing
	// contracts but no contracts will be formed/renewed/refreshed.
	Enabled bool `json:"enabled"`

	// Period is the number of blocks between a new contract's formation and
	// proof height. It needs to be greater than the RenewWindow.
	Period uint64 `json:"period"`

	// RenewWindow is the number of blocks before a contract reaches its
	// proof height where we start trying to renew it.
	RenewWindow uint64 `json:"renewWindow"`

	// WantedContracts is the number of good-for-upload contracts the
	// contract manager should maintain. e.g. if a host runs out of storage,
	// its contract(s) won't count towards this number but will still be
	// considered good for renewing/refreshing and funding accounts.
	WantedContracts uint64 `json:"wantedContracts"`
}
