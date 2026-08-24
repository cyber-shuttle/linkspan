package checkpoint

import "fmt"

/*
NetworkPolicy decides what happens to a checkpointed process's network state.

This has to be an explicit choice rather than a constant. Most linkspan
sessions reconstruct their networking in the new allocation — a fresh tunnel,
a fresh SSH server, a fresh Jupyter port — and asking CRIU to carry established
TCP connections across that is at best wasted work and at worst a dump that
fails on sockets nobody intended to migrate. Long-lived connections that really
must survive are the exception, so they are the flag.
*/
type NetworkPolicy string

const (
	// NetworkReconstruct dumps without --tcp-established: sockets are expected
	// to be rebuilt by the new allocation.
	NetworkReconstruct NetworkPolicy = "reconstruct"

	// NetworkMigrate passes --tcp-established so CRIU carries established TCP
	// connections through the checkpoint.
	NetworkMigrate NetworkPolicy = "migrate"
)

// DefaultNetworkPolicy is reconstruction, matching how linkspan actually
// restores a session.
const DefaultNetworkPolicy = NetworkReconstruct

// ValidateNetworkPolicy rejects an unknown policy at startup, where the error
// is cheap and obvious.
func ValidateNetworkPolicy(policy string) error {
	switch NetworkPolicy(policy) {
	case "", NetworkReconstruct, NetworkMigrate:
		return nil
	default:
		return fmt.Errorf("unknown network policy %q, expected %q or %q", policy, NetworkReconstruct, NetworkMigrate)
	}
}

// criuArgs returns the CRIU flags this policy implies, for both dump and
// restore — they have to agree, or a migrated checkpoint restores wrong.
func (p NetworkPolicy) criuArgs() []string {
	if p == NetworkMigrate {
		return []string{"--tcp-established"}
	}
	return nil
}
