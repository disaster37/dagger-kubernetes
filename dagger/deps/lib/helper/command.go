package helper

import (
	"fmt"
	"strings"
)

// ForgeCommand permit to forge command
func ForgeCommand(cmd string) []string {
	return strings.Split(cmd, " ")
}

// ForgeCommandf permit to forge command with any arguments
func ForgeCommandf(cmd string, params ...any) []string {
	return ForgeCommand(fmt.Sprintf(cmd, params...))
}

// ForgeScript permit to forge script
func ForgeScript(script string, params ...any) []string {
	return []string{
		"/bin/sh",
		"-c",
		fmt.Sprintf(script, params...),
	}
}