package contracts

import (
	"context"
	"fmt"
	"time"

	proto "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	rhp "go.sia.tech/coreutils/rhp/v4"
	"go.sia.tech/indexd/accounts"
	"go.sia.tech/indexd/hosts"
	"go.uber.org/zap"
)

const (
	// attachValidity is the replay window for pool attachment signatures.
	attachValidity = 5 * time.Minute

	// serviceAccountFundTargetBytes is the number of bytes used to calculate
	// the fund target for a host's service account. We fund accounts to cover
	// this amount of read and write usage. It roughly comes down to uploading
	// and downloading to and from a host at ~1Gbps for a period of 2 minutes.
	// With 30 good hosts, this results in about 30Gbps of maximum theoretical
	// throughput.
	serviceAccountFundTargetBytes = uint64(16 << 30) // 16 GiB
)

// AttachPools attaches all pending pool attachments for the given host.
func (cm *ContractManager) AttachPools(ctx context.Context, hostKey types.PublicKey, log *zap.Logger) error {
	var exhausted bool
	for !exhausted {
		// fetch pending attachments
		pending, err := cm.accounts.PendingPoolAttachments(hostKey, proto.MaxAccountBatchSize)
		if err != nil {
			return fmt.Errorf("failed to fetch pending attachments: %w", err)
		} else if len(pending) == 0 {
			break
		} else if len(pending) < proto.MaxAccountBatchSize {
			exhausted = true
		}

		// construct attach inputs
		inputs := make([]rhp.PoolAttachInput, len(pending))
		for i, p := range pending {
			inputs[i] = rhp.PoolAttachInput{
				Account: p.AccountKey,
				PoolKey: p.PoolKey,
			}
		}

		// attach pools
		if err := cm.accountFunder.AttachPools(ctx, hostKey, inputs, attachValidity); err != nil {
			return fmt.Errorf("failed to attach pools: %w", err)
		} else if err := cm.accounts.InsertPoolAttachments(hostKey, pending); err != nil {
			return fmt.Errorf("failed to record attachments: %w", err)
		}

		log.Debug("attached pools to accounts", zap.Int("count", len(pending)))
	}

	return cm.attachSharingPools(ctx, hostKey, log)
}

// attachSharingPools attaches each funded pool's derived sharing account on the
// host.
func (cm *ContractManager) attachSharingPools(ctx context.Context, hostKey types.PublicKey, log *zap.Logger) error {
	var exhausted bool
	for !exhausted {
		sharing, err := cm.accounts.SharingPoolAttachments(hostKey, proto.MaxAccountBatchSize)
		if err != nil {
			return fmt.Errorf("failed to fetch sharing pool attachments: %w", err)
		} else if len(sharing) == 0 {
			break
		} else if len(sharing) < proto.MaxAccountBatchSize {
			exhausted = true
		}

		// construct attach inputs
		inputs := make([]rhp.PoolAttachInput, len(sharing))
		for i, p := range sharing {
			inputs[i] = rhp.PoolAttachInput{
				Account: p.AccountKey,
				PoolKey: p.PoolKey,
			}
		}

		// attach sharing accounts and record them
		if err := cm.accountFunder.AttachPools(ctx, hostKey, inputs, attachValidity); err != nil {
			return fmt.Errorf("failed to attach sharing pools: %w", err)
		} else if err := cm.accounts.MarkSharingPoolsAttached(hostKey, sharing); err != nil {
			return fmt.Errorf("failed to record sharing attachments: %w", err)
		}

		log.Debug("attached sharing accounts to pools", zap.Int("count", len(sharing)))
	}
	return nil
}

// ContractFundTarget calculates the fund target for a contract on the given
// host. For hosts that support pools it sums per-quota pool targets, for
// legacy hosts it sums per-quota per-account targets. One active pool counts
// the same as one active legacy account. Service accounts are always
// included.
func (cm *ContractManager) ContractFundTarget(ctx context.Context, host hosts.Host, minAllowance types.Currency) (types.Currency, error) {
	threshold := time.Now().Add(-accounts.AccountActivityThreshold)

	var infos []accounts.QuotaFundInfo
	var err error
	if host.HasPoolSupport() {
		infos, err = cm.accounts.PoolFundingInfo(threshold)
	} else {
		infos, err = cm.accounts.AccountFundingInfo(threshold)
	}
	if err != nil {
		return types.ZeroCurrency, err
	}

	var target types.Currency
	for _, info := range infos {
		fullStorage := min(info.FullStorage, info.Active)
		upload := info.Active - fullStorage
		t := accounts.HostFundTarget(host, info.FundTargetBytes).Mul64(upload)
		t = t.Add(accounts.HostReadFundTarget(host, info.FundTargetBytes).Mul64(fullStorage))
		target = target.Add(t)
	}

	// service accounts
	target = target.Add(accounts.HostFundTarget(host, serviceAccountFundTargetBytes).Mul64(uint64(len(cm.accounts.ServiceAccounts(host.PublicKey)))))

	if target.Cmp(minAllowance) < 0 {
		target = minAllowance
	}

	return target, nil
}

// FundAccounts funds individual accounts on legacy hosts. For hosts that support pools, FundPools is used.
func (cm *ContractManager) FundAccounts(ctx context.Context, host hosts.Host, contractIDs []types.FileContractID, quotas []accounts.Quota, log *zap.Logger) error {
	if len(contractIDs) == 0 {
		log.Debug("no contracts provided")
		return nil
	} else if host.Blocked {
		log.Debug("host is blocked")
		return nil
	} else if !host.Usability.Usable() {
		log.Debug("host is not usable")
		return nil
	} else if host.HasPoolSupport() {
		return nil
	}

	threshold := time.Now().Add(-accounts.AccountActivityThreshold)

	// Budget-enforced candidates need a fresh SC/USD rate to price their
	// deposits; without one we skip them (fail closed) and keep funding legacy
	// candidates. The rate is captured here and reused when recording spend so
	// a rate that goes stale mid-pass cannot drop it after the deposit.
	rate, rateOK := cm.scUSDRate()
OUTER:
	for _, quota := range quotas {
		fundTarget := accounts.HostFundTarget(host, quota.FundTargetBytes)
		readFundTarget := accounts.HostReadFundTarget(host, quota.FundTargetBytes)
		if fundTarget.IsZero() && readFundTarget.IsZero() {
			continue
		}

		var exhausted bool
		for !exhausted {
			accs, err := cm.accounts.AccountsForFunding(host.PublicKey, quota.Key, threshold, accounts.AccountFundBatch)
			if err != nil {
				return fmt.Errorf("failed to fetch accounts for funding: %w", err)
			} else if len(accs) < accounts.AccountFundBatch {
				exhausted = true
			}
			if len(accs) == 0 {
				break
			}

			// split accounts by storage state
			var uploadAccs, fullStorageAccs []accounts.HostAccount
			for _, acc := range accs {
				if acc.FullStorage {
					fullStorageAccs = append(fullStorageAccs, acc)
				} else {
					uploadAccs = append(uploadAccs, acc)
				}
			}

			// fund each group with the appropriate target
			for _, batch := range []struct {
				accs   []accounts.HostAccount
				target types.Currency
			}{
				{uploadAccs, fundTarget},
				{fullStorageAccs, readFundTarget},
			} {
				if len(batch.accs) == 0 || batch.target.IsZero() {
					continue
				}

				batchAccs := batch.accs
				if !rateOK {
					var skipped []accounts.HostAccount
					batchAccs = make([]accounts.HostAccount, 0, len(batch.accs))
					for _, acc := range batch.accs {
						if acc.Enforced {
							skipped = append(skipped, acc)
						} else {
							batchAccs = append(batchAccs, acc)
						}
					}
					if len(skipped) > 0 {
						log.Warn("SC/USD rate unavailable, skipping budget-enforced accounts",
							zap.String("quota", quota.Key), zap.Int("skipped", len(skipped)))
						// defer the skipped candidates so the candidate query
						// makes progress; a full batch of enforced candidates
						// would otherwise be re-fetched forever. A candidate
						// with no account_hosts row has never been funded, so
						// record a failure: readiness only counts rows with
						// consecutive_failed_funds = 0 and a never-funded
						// account must not look ready during an outage. An
						// existing (already funded) account keeps its counter
						// and stays ready while its funding is deferred.
						now := time.Now()
						for i := range skipped {
							skipped[i].NextFund = now.Add(accounts.AccountFundInterval)
							if !skipped[i].Existing {
								skipped[i].ConsecutiveFailedFunds = 1
							}
						}
						if err := cm.accounts.UpdateHostAccounts(skipped); err != nil {
							return fmt.Errorf("failed to defer accounts awaiting a rate: %w", err)
						}
					}
					if len(batchAccs) == 0 {
						continue
					}
				}

				deposits, funded, drained, err := cm.accountFunder.FundAccounts(ctx, host, contractIDs, batchAccs, batch.target, log)
				if err != nil {
					return fmt.Errorf("failed to fund accounts: %w", err)
				}

				// record spend before the bookkeeping updates below: the
				// deposit already happened, so a failure in
				// UpdateFundedAccounts/UpdateHostAccounts must not drop it
				if rateOK {
					cm.recordAccountFundingSpend(ctx, batchAccs[:funded], deposits, rate, log)
				}

				accounts.UpdateFundedAccounts(batchAccs, funded, cm.maxAccountFundingBackoff)
				if err := cm.accounts.UpdateHostAccounts(batchAccs); err != nil {
					return fmt.Errorf("failed to update accounts: %w", err)
				}

				contractIDs = contractIDs[drained:]
				if len(contractIDs) == 0 {
					log.Debug("not all accounts could be funded, no more contracts available", zap.String("quota", quota.Key))
					break OUTER
				}
			}
		}
	}

	return nil
}

// FundServiceAccounts funds service accounts on any host.
func (cm *ContractManager) FundServiceAccounts(ctx context.Context, host hosts.Host, contractIDs []types.FileContractID, log *zap.Logger) error {
	if len(contractIDs) == 0 {
		log.Debug("no contracts provided")
		return nil
	} else if host.Blocked {
		log.Debug("host is blocked")
		return nil
	} else if !host.Usability.Usable() {
		log.Debug("host is not usable")
		return nil
	}

	// fund service accounts
	serviceAccounts := cm.accounts.ServiceAccounts(host.PublicKey)
	if len(serviceAccounts) > 0 {
		fundTarget := accounts.HostFundTarget(host, serviceAccountFundTargetBytes)
		_, funded, _, err := cm.accountFunder.FundAccounts(ctx, host, contractIDs, serviceAccounts, fundTarget, log)
		if err != nil {
			return fmt.Errorf("failed to fund service accounts: %w", err)
		}

		if err := cm.accounts.UpdateServiceAccounts(serviceAccounts[:funded], fundTarget); err != nil {
			cm.log.Warn("failed to update service account balances", zap.Error(err))
		}
	}
	return nil
}

// FundPools attempts to fund all pools for the given host key. It does so
// using the provided contract IDs, which are used in the order they're given.
// Only hosts that support pools (>= 5.1.0) are funded.
func (cm *ContractManager) FundPools(ctx context.Context, host hosts.Host, contractIDs []types.FileContractID, quotas []accounts.Quota, log *zap.Logger) error {
	if !host.HasPoolSupport() {
		log.Debug("host does not support pools", zap.Stringer("version", host.Settings.ProtocolVersion))
		return nil
	} else if len(contractIDs) == 0 {
		log.Debug("no contracts provided")
		return nil
	} else if host.Blocked {
		log.Debug("host is blocked")
		return nil
	} else if !host.Usability.Usable() {
		log.Debug("host is not usable")
		return nil
	}

	threshold := time.Now().Add(-accounts.AccountActivityThreshold)

	// Budget-enforced pools need a fresh SC/USD rate to price their deposits;
	// without one we skip them (fail closed) and keep funding legacy pools.
	// The rate is captured here and reused when recording spend so a rate that
	// goes stale mid-pass cannot drop it after the deposit.
	rate, rateOK := cm.scUSDRate()
OUTER:
	for _, quota := range quotas {
		fundTarget := accounts.HostFundTarget(host, quota.FundTargetBytes)
		readFundTarget := accounts.HostReadFundTarget(host, quota.FundTargetBytes)
		if fundTarget.IsZero() && readFundTarget.IsZero() {
			continue
		}

		var exhausted bool
		for !exhausted {
			pools, err := cm.accounts.PoolsForFunding(host.PublicKey, quota.Key, threshold, proto.MaxAccountBatchSize)
			if err != nil {
				return fmt.Errorf("failed to fetch pools for funding: %w", err)
			} else if len(pools) < proto.MaxAccountBatchSize {
				exhausted = true
			}
			if len(pools) == 0 {
				break
			}

			// split pools by storage state
			var uploadPools, fullStoragePools []accounts.HostPool
			for _, p := range pools {
				if p.FullStorage {
					fullStoragePools = append(fullStoragePools, p)
				} else {
					uploadPools = append(uploadPools, p)
				}
			}

			for _, batch := range []struct {
				pools  []accounts.HostPool
				target types.Currency
			}{
				{uploadPools, fundTarget},
				{fullStoragePools, readFundTarget},
			} {
				if len(batch.pools) == 0 || batch.target.IsZero() {
					continue
				}

				batchPools := batch.pools
				if !rateOK {
					var skipped []accounts.HostPool
					batchPools = make([]accounts.HostPool, 0, len(batch.pools))
					for _, pool := range batch.pools {
						if pool.Enforced {
							skipped = append(skipped, pool)
						} else {
							batchPools = append(batchPools, pool)
						}
					}
					if len(skipped) > 0 {
						log.Warn("SC/USD rate unavailable, skipping budget-enforced pools",
							zap.String("quota", quota.Key), zap.Int("skipped", len(skipped)))
						// defer the skipped pools so the candidate query makes
						// progress; a full batch of enforced pools would
						// otherwise be re-fetched forever. A pool with no
						// pool_hosts row has never been funded, so record a
						// failure: readiness only counts rows with
						// consecutive_failed_funds = 0 and a never-funded
						// pool must not look ready during an outage. An
						// existing (already funded) pool keeps its counter
						// and stays ready while its funding is deferred.
						now := time.Now()
						for i := range skipped {
							skipped[i].NextFund = now.Add(accounts.PoolFundInterval)
							if !skipped[i].Existing {
								skipped[i].ConsecutiveFailedFunds = 1
							}
						}
						if err := cm.accounts.UpdateHostPools(skipped); err != nil {
							return fmt.Errorf("failed to defer pools awaiting a rate: %w", err)
						}
					}
					if len(batchPools) == 0 {
						continue
					}
				}

				deposits, funded, drained, err := cm.accountFunder.FundPools(ctx, host, contractIDs, batchPools, batch.target, log)
				if err != nil {
					return fmt.Errorf("failed to fund pools: %w", err)
				}

				// record spend before the bookkeeping updates below: the
				// deposit already happened, so a failure in
				// UpdateFundedPools/UpdateHostPools must not drop it
				if rateOK {
					cm.recordPoolFundingSpend(ctx, batchPools[:funded], deposits, rate, log)
				}

				accounts.UpdateFundedPools(batchPools, funded, cm.maxAccountFundingBackoff)
				if err := cm.accounts.UpdateHostPools(batchPools); err != nil {
					return fmt.Errorf("failed to update pools: %w", err)
				}

				contractIDs = contractIDs[drained:]
				if len(contractIDs) == 0 {
					log.Debug("not all pools could be funded, no more contracts available", zap.String("quota", quota.Key))
					break OUTER
				}
			}
		}
	}

	return nil
}

// recordAccountFundingSpend converts the deposits returned for the funded
// budget-enforced accounts to USD at the given (captured) rate and atomically
// adds them to each account's funding spend. Legacy accounts and accounts that
// received no deposit are skipped. Failures are logged at error level: the
// deposit already happened, so a lost spend record must be visible.
func (cm *ContractManager) recordAccountFundingSpend(ctx context.Context, accs []accounts.HostAccount, deposits map[types.PublicKey]types.Currency, rate float64, log *zap.Logger) {
	for _, acc := range accs {
		if !acc.Enforced {
			continue
		}
		key := types.PublicKey(acc.AccountKey)
		if usd := deposits[key].Siacoins() * rate; usd > 0 {
			if err := cm.accounts.AddAccountFundingSpend(ctx, key, usd); err != nil {
				log.Error("failed to record account funding spend",
					zap.Stringer("accountKey", key), zap.Float64("usd", usd), zap.Error(err))
			}
		}
	}
}

// recordPoolFundingSpend converts the deposits returned for the funded
// budget-enforced pools to USD at the given (captured) rate and atomically
// adds them to the spend of the budget-enforced accounts on each pool's
// connect key. Legacy pools and pools that received no deposit are skipped.
// Failures are logged at error level: the deposit already happened, so a lost
// spend record must be visible.
func (cm *ContractManager) recordPoolFundingSpend(ctx context.Context, pools []accounts.HostPool, deposits map[types.PublicKey]types.Currency, rate float64, log *zap.Logger) {
	for _, pool := range pools {
		if !pool.Enforced {
			continue
		}
		key := pool.PoolKey.PublicKey()
		if usd := deposits[key].Siacoins() * rate; usd > 0 {
			if err := cm.accounts.AddPoolFundingSpend(ctx, key, usd); err != nil {
				log.Error("failed to record pool funding spend",
					zap.Stringer("poolKey", key), zap.Float64("usd", usd), zap.Error(err))
			}
		}
	}
}
