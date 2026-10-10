package backend

import "testing"

func TestPhase3ESettingsSaveCannotBypassSourceDelete(t *testing.T) {
	withTask6HTTPFixture(t, nil, nil, func(f *task6HTTPFixture) {
		cfg := f.app.ConfigStore.Snapshot()
		current := phase3ESourceVersion(f.app.IDStore, "server-b")
		cfg.Upstream = cfg.Upstream[:1]
		if err := f.app.commitConfigSettingsOnly(cfg); err == nil {
			t.Fatal("generic settings save bypassed source delete lifecycle")
		}
		if got := phase3ESourceVersion(f.app.IDStore, "server-b"); got != current {
			t.Fatal("rejected config transaction changed generation", got)
		}
		if len(f.app.ConfigStore.Snapshot().Upstream) != 2 {
			t.Fatal("rejected config transaction changed upstream list")
		}
	})
}

func TestPhase3EStandaloneRemoveAlsoAdvancesEpoch(t *testing.T) {
	dir := t.TempDir()
	s, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.GetOrCreateVirtualID("item-a", "server-a")
	if err := s.RemoveByServerID("server-a"); err != nil {
		t.Fatal(err)
	}
	if version := phase3ESourceVersion(s, "server-a"); version != 2 {
		t.Fatal("standalone delete epoch", version)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if version := phase3ESourceVersion(s, "server-a"); version != 2 {
		t.Fatal("standalone epoch not persisted", version)
	}
}
