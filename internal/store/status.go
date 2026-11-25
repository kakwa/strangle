package store

// HostStatus derives a host's overall status from its recap counts: failed
// and unreachable win over changed, which wins over ok.
func HostStatus(ok, changed, failed, unreachable int) string {
	switch {
	case failed > 0:
		return "failed"
	case unreachable > 0:
		return "unreachable"
	case changed > 0:
		return "changed"
	default:
		return "ok"
	}
}
