package linear

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func fixture() *State {
	s := &State{}
	s.Init()
	s.Connections["c"] = &Connection{ID: "c", Teams: []Resource{{ID: "t"}, {ID: "other"}}}
	return s
}
func profile(name, project string) Profile {
	return Profile{ConnectionID: "c", Name: name, TeamID: "t", LinearProjectID: project, LocalProjectID: "local", RunnerID: name, Challenge: Hash(name)}
}
func TestProfileConflictsAndOwnership(t *testing.T) {
	s := fixture()
	p, err := s.SaveProfile(profile("Café", ""), "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{" café ", "CAFE\u0301", "ＣＡＦÉ"} {
		_, err = s.SaveProfile(profile(name, "project"), "owner")
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("%q: %v", name, err)
		}
	}
	if _, err = s.SaveProfile(profile("Other name", ""), "owner"); !errors.Is(err, ErrConflict) {
		t.Fatal("overlapping team route accepted")
	}
	renamed := *p
	renamed.Name = "Backend"
	if _, err = s.SaveProfile(renamed, "intruder"); !errors.Is(err, ErrInvalid) {
		t.Fatal("ownership changed")
	}
	renamed.LocalProjectID = "different"
	if _, err = s.SaveProfile(renamed, "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatal("active tasks could move runners")
	}
	renamed = *p
	renamed.Name = "Backend"
	saved, err := s.SaveProfile(renamed, "owner")
	if err != nil || saved.ID != p.ID || saved.Challenge != p.Challenge {
		t.Fatal("rename replaced identity", err)
	}
	invalid := profile("x", "project")
	invalid.TeamID = "forbidden"
	if _, err = s.SaveProfile(invalid, "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatal("ungranted team accepted")
	}
}
func TestCreateRetryAndServerOwnedFields(t *testing.T) {
	s := fixture()
	in := profile("AO", "")
	in.LastSeen = time.Now()
	in.Disconnected = true
	p, err := s.SaveProfile(in, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if !p.LastSeen.IsZero() || p.Disconnected {
		t.Fatal("client forged runner state")
	}
	retried, err := s.SaveProfile(in, "owner")
	if err != nil || p.ID != retried.ID || len(s.Profiles) != 1 {
		t.Fatal("retry duplicated profile", err)
	}
}
func TestRoutingPauseAndLeaseFencing(t *testing.T) {
	s := fixture()
	fallback, _ := s.SaveProfile(profile("All", ""), "owner")
	exact, _ := s.SaveProfile(profile("Project", "p"), "owner")
	s.Enqueue("task", "c", "t", "p", "create", "create", "hello")
	if s.Tasks["task"].ProfileID != exact.ID {
		t.Fatal("exact project did not win")
	}
	command := s.Claim(exact.ID, time.Now())
	if command == nil {
		t.Fatal("no command")
	}
	old := *command
	if s.Claim(exact.ID, time.Now()) != nil || s.Check(fallback.ID, old, true, time.Now()) {
		t.Fatal("lease isolation failed")
	}
	renamed := *exact
	renamed.Name = "Renamed"
	renamed.Paused = true
	s.SaveProfile(renamed, "owner")
	s.Enqueue("task", "c", "other", "different", "followup", "message", "follow up")
	if s.Tasks["task"].ProfileID != exact.ID {
		t.Fatal("routing edit moved active task")
	}
	s.Enqueue("new", "c", "t", "p", "create-new", "create", "must not run")
	if s.Tasks["new"].ProfileID != "" {
		t.Fatal("paused exact route fell through to team fallback")
	}
	s.Enqueue("task", "c", "t", "p", "stop", "stop", "")
	if s.Check(exact.ID, old, false, time.Now()) {
		t.Fatal("stop did not fence prompt")
	}
	stop := s.Claim(exact.ID, time.Now())
	if stop == nil || stop.Kind != "stop" {
		t.Fatal("stop not delivered")
	}
	s.Enqueue("task", "c", "t", "p", "later", "message", "ignored")
	if len(s.Tasks["task"].Commands) != 3 {
		t.Fatal("message resurrected stopped task")
	}
	s.Report(Report{ID: "r", Task: "task", Kind: "response", Body: "done"})
	id := s.Outbox["r"].ProviderID
	s.Report(Report{ID: "r", Task: "task", Body: "duplicate"})
	if s.Outbox["r"].ProviderID != id || !strings.Contains(s.Outbox["r"].Report.Body, "done") {
		t.Fatal("report replay changed delivery")
	}
}
