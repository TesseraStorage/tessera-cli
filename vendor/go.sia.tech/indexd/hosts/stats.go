package hosts

// AggregatedHostStats includes aggregated statistics about all hosts, including
// the number of active hosts and scan counts.
type AggregatedHostStats struct {
	Active        uint64 `json:"active"`
	GoodForUpload uint64 `json:"goodForUpload"`
	TotalScans    int64  `json:"totalScans"`
	FailedScans   int64  `json:"failedScans"`
}

// AggregatedHostStats reports aggregated statistics about all hosts, including
// the number of active hosts and scan counts.
func (m *HostManager) AggregatedHostStats() (AggregatedHostStats, error) {
	return m.store.AggregatedHostStats()
}
