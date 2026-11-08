package main

// Stand-in for the rules commands (rules.go, personal.go, hygiene.go,
// scan.go, cmd_rules.go), which replace this file.

func rulesCommand() *cliCmd        { return newCmd("rules") }
func runRules(m *cliMatches) error { return errf("not built yet") }
