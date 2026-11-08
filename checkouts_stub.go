package main

// Stand-in for checkouts, relate and carry (checkouts.go, facts.go,
// relate.go, carry.go and their cmd_*.go), which replace this file.

func checkoutsCommand() *cliCmd        { return newCmd("checkouts") }
func runCheckouts(m *cliMatches) error { return errf("not built yet") }
func relateCommand() *cliCmd           { return newCmd("relate") }
func runRelate(m *cliMatches) error    { return errf("not built yet") }
func carryCommand() *cliCmd            { return newCmd("carry") }
func runCarry(m *cliMatches) error     { return errf("not built yet") }
