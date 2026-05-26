package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
)

// ErrUnencryptedOverlay indicates one or more overlay networks attached
// to our container are not encrypted. Manager-replica traffic on such a
// network is plaintext on the wire between hosts. The caller decides
// whether to fail-closed (preferred) or warn-and-continue (escape
// hatch).
var ErrUnencryptedOverlay = errors.New("unencrypted overlay network attached")

// CheckOverlayEncryption inspects every network attached to our own
// container and returns ErrUnencryptedOverlay if any of them is an
// overlay without IPsec enabled. Non-overlay networks (bridge, host,
// none) are ignored — they don't traverse the wire between hosts.
//
// The returned error wraps ErrUnencryptedOverlay and embeds the names of
// the offending networks so operators can read the log line and act.
func CheckOverlayEncryption(ctx context.Context, dcli Client) error {
	names, err := dcli.SelfAttachedNetworks(ctx)
	if err != nil {
		return fmt.Errorf("list attached networks: %w", err)
	}
	var unencrypted []string
	for _, name := range names {
		summary, inspectErr := dcli.NetworkInspect(ctx, name)
		if inspectErr != nil {
			return fmt.Errorf("inspect network %q: %w", name, inspectErr)
		}
		if summary.Driver != "overlay" {
			continue
		}
		if !summary.Encrypted {
			unencrypted = append(unencrypted, summary.Name)
		}
	}
	if len(unencrypted) > 0 {
		slices.Sort(unencrypted)
		return fmt.Errorf("%w: %v (set driver_opts.encrypted: \"true\" on each)",
			ErrUnencryptedOverlay, unencrypted)
	}
	return nil
}

// LogUnencryptedOverlayWarning emits the loud warning we surface when the
// operator explicitly disables the encryption gate via the escape-hatch
// env var. Centralized here so the message is consistent across the
// codepaths that opt out.
func LogUnencryptedOverlayWarning(err error) {
	slog.Warn(
		"OVERLAY ENCRYPTION DISABLED — inter-host coordinator traffic is plaintext",
		"err", err.Error(),
		"how_to_fix", "set driver_opts.encrypted: \"true\" on the coordinator's overlay network",
	)
}
