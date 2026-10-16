package lock

import (
	"context"
	"fmt"
	"strconv"
)

// Labels on the lock Config that record a coordinator-driven drain of the
// holder. Labels, not the Config's data: data is immutable once created and
// is the HTTP-visible lock document, while labels can be updated in place
// with an optimistic version check. They live and die with the lock
// instance, so a stolen or released lock never carries a stale mark.
const (
	labelPrefix      = "swarm-auto-drain."
	labelNodeID      = labelPrefix + "drained-node-id"
	labelDrainedAt   = labelPrefix + "drained-at"
	labelDownSeenAt  = labelPrefix + "down-seen-at"
	labelActivatedAt = labelPrefix + "activated-at"
)

// DrainMark records that the coordinator drained the lock holder for a
// reboot, and how far the leader's reactivation has progressed. Timestamps
// are Unix seconds UTC; zero means "not yet".
type DrainMark struct {
	NodeID      string
	DrainedAt   int64
	DownSeenAt  int64
	ActivatedAt int64
}

// Snapshot is one lock instance as read from Swarm: the document, the
// drain mark (nil when the holder was not drained by the coordinator), and
// the Config's ID and version for instance-exact updates and removal.
type Snapshot struct {
	Lock    *Lock
	Mark    *DrainMark
	ID      string
	Version uint64
}

// Snapshot reads the current lock instance. Returns ErrNoLock if none.
//
// The ID and version are read first and the document is then read by ID,
// so all fields describe the same instance. If the labels change between
// the two reads, Version is older than Mark and the next UpdateMark fails
// its version check rather than overwriting the newer labels.
func (s *ConfigStore) Snapshot(ctx context.Context) (*Snapshot, error) {
	id, version, err := s.Docker.ConfigMeta(ctx, s.ConfigName)
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNoLock
		}
		return nil, fmt.Errorf("config meta: %w", err)
	}
	doc, labels, err := s.loadByRef(ctx, id)
	if err != nil {
		return nil, err
	}
	return &Snapshot{Lock: doc, Mark: decodeMark(labels), ID: id, Version: version}, nil
}

// MarkDrained records on the current lock that the coordinator is draining
// `holder` (node `nodeID`) for a reboot. Returns ErrNoLock or ErrNotHolder
// when the caller does not hold the lock; nothing is written then. A mark
// left by an earlier drain of the same lock instance is replaced.
func (s *ConfigStore) MarkDrained(ctx context.Context, holder, nodeID string) error {
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	if snap.Lock.Holder != holder {
		return ErrNotHolder
	}
	mark := DrainMark{NodeID: nodeID, DrainedAt: s.now().UTC().Unix(), DownSeenAt: 0, ActivatedAt: 0}
	return s.UpdateMark(ctx, snap, mark)
}

// UpdateMark replaces the drain mark of the instance described by snap.
// Fails if that instance is gone or its labels changed since snap was read.
func (s *ConfigStore) UpdateMark(ctx context.Context, snap *Snapshot, mark DrainMark) error {
	if err := s.Docker.ConfigUpdateLabels(ctx, snap.ID, snap.Version, encodeMark(mark)); err != nil {
		if isNotFound(err) {
			return ErrNoLock
		}
		return fmt.Errorf("update drain mark: %w", err)
	}
	return nil
}

// ReleaseSnapshot removes exactly the lock instance described by snap, by
// Config ID. A lock that was released, stolen or re-acquired since snap was
// read has a different ID and is left alone; ErrNoLock reports that case.
func (s *ConfigStore) ReleaseSnapshot(ctx context.Context, snap *Snapshot) error {
	if err := s.Docker.ConfigRemove(ctx, snap.ID); err != nil {
		if isNotFound(err) {
			s.recordReleaseOutcome(ctx, "not_found")
			return ErrNoLock
		}
		s.recordReleaseOutcome(ctx, "error")
		return fmt.Errorf("remove: %w", err)
	}
	s.recordReleaseOutcome(ctx, "reactivated")
	return nil
}

// loadByRef reads and parses the lock document and labels by Config name
// or ID.
func (s *ConfigStore) loadByRef(ctx context.Context, ref string) (*Lock, map[string]string, error) {
	data, labels, err := s.Docker.ConfigInspect(ctx, ref)
	if err != nil {
		if isNotFound(err) {
			return nil, nil, ErrNoLock
		}
		return nil, nil, fmt.Errorf("config inspect: %w", err)
	}
	doc, err := parseLock(data)
	if err != nil {
		return nil, nil, err
	}
	return doc, labels, nil
}

func encodeMark(m DrainMark) map[string]string {
	labels := map[string]string{
		labelNodeID:    m.NodeID,
		labelDrainedAt: strconv.FormatInt(m.DrainedAt, 10),
	}
	if m.DownSeenAt != 0 {
		labels[labelDownSeenAt] = strconv.FormatInt(m.DownSeenAt, 10)
	}
	if m.ActivatedAt != 0 {
		labels[labelActivatedAt] = strconv.FormatInt(m.ActivatedAt, 10)
	}
	return labels
}

// decodeMark returns nil unless the labels carry a complete, well-formed
// mark. A malformed mark is treated as absent: the reactivation loop then
// leaves the node alone, which is the safe failure.
func decodeMark(labels map[string]string) *DrainMark {
	nodeID := labels[labelNodeID]
	if nodeID == "" {
		return nil
	}
	drainedAt, ok := parseTimestamp(labels, labelDrainedAt)
	if !ok || drainedAt == 0 {
		return nil
	}
	downSeenAt, ok := parseTimestamp(labels, labelDownSeenAt)
	if !ok {
		return nil
	}
	activatedAt, ok := parseTimestamp(labels, labelActivatedAt)
	if !ok {
		return nil
	}
	return &DrainMark{NodeID: nodeID, DrainedAt: drainedAt, DownSeenAt: downSeenAt, ActivatedAt: activatedAt}
}

// parseTimestamp reads an optional Unix-seconds label; absent is (0, true).
func parseTimestamp(labels map[string]string, key string) (int64, bool) {
	raw, present := labels[key]
	if !present {
		return 0, true
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}
