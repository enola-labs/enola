package vocab

import "testing"

// Aliases choose edges, so two snapshots linked under different aliases must not
// fingerprint alike. Declaring none must leave the fingerprint exactly as it was.
func TestFingerprint_ServiceAliases(t *testing.T) {
	base := Default().Fingerprint()

	empty := Default()
	empty.ServiceAliases = map[string]string{}
	if empty.Fingerprint() != base {
		t.Error("an empty alias map changed the fingerprint")
	}

	toGateway := Default()
	toGateway.ServiceAliases = map[string]string{"resource-api": "gateway"}
	if toGateway.Fingerprint() == base {
		t.Error("an alias did not change the fingerprint")
	}

	toReplica := Default()
	toReplica.ServiceAliases = map[string]string{"resource-api": "replica"}
	if toReplica.Fingerprint() == toGateway.Fingerprint() {
		t.Error("aliases to different repositories fingerprint alike")
	}
}
