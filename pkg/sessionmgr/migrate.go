package sessionmgr

import (
	"crypto/rand"
	"encoding/hex"
)

// migrateToCurrentVersion brings entries to the current format version.
// Mutates the slice in place; returns true if any migration was applied.
func migrateToCurrentVersion(entries []Entry) bool {
	version := 1
	for i := range entries {
		if entries[i].Type == EntrySession {
			version = entries[i].Version
			break
		}
	}
	if version >= CurrentSessionVersion {
		return false
	}
	if version < 2 {
		migrateV1ToV2(entries)
	}
	if version < 3 {
		migrateV2ToV3(entries)
	}
	return true
}

// migrateV1ToV2 adds id/parentId tree structure and converts compaction
// firstKeptEntryIndex → firstKeptEntryId. Mutates in place.
func migrateV1ToV2(entries []Entry) {
	ids := map[string]bool{}
	var prevID *string
	for i := range entries {
		e := &entries[i]
		if e.Type == EntrySession {
			e.Version = 2
			continue
		}
		id := generateIDForMigration(ids)
		ids[id] = true
		e.ID = id
		e.ParentID = prevID
		p := id
		prevID = &p
		if e.Type == EntryCompaction && e.FirstKeptEntryIdx != nil {
			if idx := *e.FirstKeptEntryIdx; idx >= 0 && idx < len(entries) && entries[idx].Type != EntrySession {
				e.FirstKeptEntryID = entries[idx].ID
			}
			e.FirstKeptEntryIdx = nil
		}
	}
}

// migrateV2ToV3 renames hookMessage role to custom.
func migrateV2ToV3(entries []Entry) {
	for i := range entries {
		e := &entries[i]
		if e.Type == EntrySession {
			e.Version = 3
			continue
		}
		if e.Type == EntryMessage && e.Message != nil && e.Message.Role == "hookMessage" {
			e.Message.Role = "custom"
		}
	}
}

func generateIDForMigration(ids map[string]bool) string {
	for i := 0; i < 100; i++ {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		id := hex.EncodeToString(b)
		if !ids[id] {
			return id
		}
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
