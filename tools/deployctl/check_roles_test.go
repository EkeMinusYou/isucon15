package main

import "testing"

func TestCheckRolesAcceptsInstancedUnit(t *testing.T) {
	args := []string{"-dry-run", "-role", "all=host1", "-role", "app=host1", "-var", "service=app", "-var", "disabled_services=postgresql@16-main"}
	if err := runCheckRoles(args); err != nil {
		t.Fatal(err)
	}
	args[len(args)-1] = "disabled_services=postgresql@16-main;echo"
	if err := runCheckRoles(args); err == nil {
		t.Fatal("accepted shell metacharacters")
	}
}
