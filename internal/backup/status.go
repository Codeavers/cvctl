package backup

import "github.com/codeavers/cvctl/internal/state"

// Status returns the recorded backup status of every service of an instance.
func Status(st *state.State) map[string]state.BackupStatus {
	if st == nil || st.Backups == nil {
		return map[string]state.BackupStatus{}
	}
	return st.Backups
}
