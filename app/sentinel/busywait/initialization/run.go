/*
|    Protect your secrets, protect your sensitive data.
:    Explore VMware Secrets Manager docs at https://vsecm.com/
</
<>/  keep your secrets... secret
>/
<>/' Copyright 2023-present VMware Secrets Manager contributors.
>/'  SPDX-License-Identifier: BSD-2-Clause
*/

package initialization

import (
	"bufio"
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/vmware-tanzu/secrets-manager/app/sentinel/internal/safe"
	entity "github.com/vmware-tanzu/secrets-manager/core/entity/data/v1"
	"github.com/vmware-tanzu/secrets-manager/core/env"
	log "github.com/vmware-tanzu/secrets-manager/core/log/std"
	"github.com/vmware-tanzu/secrets-manager/core/spiffe"
	"os"
)

// RunInitCommands reads and processes initialization commands from a file.
//
// This function is designed to execute an initial set of commands that are
// declaratively defined in a file. The end result will be as if an operator
// manually entered those commands using the Sentinel CLI.
//
// The function opens the file and reads it line by line using a `bufio.Scanner`.
// Each line is expected to be a command in a specific format, typically
// key-value pairs separated by a defined separator. Lines that do not conform
// to the expected format are ignored.
//
// Special handling is applied for commands that require sleeping (pause
// execution for a specified duration) or processing a block of commands.
//
// The function supports dynamic commands, which are defined in the
// 'entity.SentinelCommand' struct.
//
// Key commands include:
// - workload: (w) Sets the WorkloadIds field in the SentinelCommand.
// - namespace: (n) Sets the Namespaces field.
// - secret: (s) Sets the Secret field.
// - transformation: (t) Sets the Template field.
// - sleep: (sleep) Enables sleep mode and sets the SleepIntervalMs field.
//
// If the file cannot be opened, the function logs an informational message and
// returns early. Errors encountered while reading the file or closing it are
// logged as errors.
func RunInitCommands(ctx context.Context) {
	cid := ctx.Value("correlationId").(*string)

	src, acquired := spiffe.AcquireSourceForSentinel(ctx)
	if !acquired {
		timeout := env.InitCommandRunnerWaitTimeoutForSentinel()

		timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

	free:
		for {
			select {
			case <-timeoutCtx.Done():
				log.ErrorLn(cid, "Failed to acquire source at RunInitCommands (1)")
				return
			case <-ticker.C:
				log.InfoLn(cid, "Acquired source (1)")
				src, acquired = spiffe.AcquireSourceForSentinel(timeoutCtx)
				if acquired {
					break free
				}
			}
		}
	}

	if src == nil {
		log.ErrorLn(cid, "Failed to acquire source at RunInitCommands (2)")
		return
	}

	// If we are here, then SPIFFE Workload API is functioning as expected.
	// We'll do one last check to ensure Sentinel can communicate with Safe
	// before executing the init commands.

	foreverCtx := context.WithoutCancel(ctx)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	canEstablishedConnectivityToSafe := false
	if src, acquired := spiffe.AcquireSourceForSentinel(foreverCtx); acquired {
		log.TraceLn(cid, "Acquired source (2)")

		if err := safe.Check(foreverCtx, src); err == nil {
			canEstablishedConnectivityToSafe = true
		}
	}

dance:
	for {
		if canEstablishedConnectivityToSafe {
			break dance
		}

		// Acquiring the source again for defensive programming.
		// At this point the cluster is (likely) still initializing.
		// Although the cached `src` should be valid and ready to be
		// used, there is no harm in acquiring a brand-new source
		// just for the sake of running initialization commands.
		src, acquired := spiffe.AcquireSourceForSentinel(foreverCtx)
		if !acquired {
			log.ErrorLn(
				cid,
				"RunInitCommands: Failed to acquire source... will retry (3)",
			)

			continue
		}

		select {
		case <-ticker.C:
			err := safe.Check(foreverCtx, src)

			if err == nil {
				break dance
			}

			log.ErrorLn(cid, "RunInitCommands: tick: error: (4)", err.Error())
		}
	}

	// Parse tombstone file first:
	tombstonePath := env.InitCommandTombstonePathForSentinel()
	file, err := os.Open(tombstonePath)
	if err != nil {
		log.InfoLn(
			cid,
			"RunInitCommands: no tombstone file found... skipping custom initialization.",
		)
		return
	}

	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			log.ErrorLn(cid, "Error closing tombstone file: ", err.Error())
		}
	}(file)

	data, err := os.ReadFile(tombstonePath)

	log.InfoLn(cid, "tombstone data '", string(data), "'")

	if strings.TrimSpace(string(data)) == "complete" {
		log.InfoLn(
			cid,
			"RunInitCommands: Already initialized. Skipping custom initialization.",
		)
		return
	}

	filePath := env.InitCommandPathForSentinel()
	file, err = os.Open(filePath)

	if err != nil {
		log.InfoLn(
			cid,
			"RunInitCommands: no initialization file found... skipping custom initialization.",
		)
		return
	}

	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			log.ErrorLn(cid, "RunInitCommands: Error closing initialization file: ", err.Error())
		}
	}(file)

	scanner := bufio.NewScanner(file)
	var sc entity.SentinelCommand

out:
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "" {
			continue
		}

		parts := strings.SplitN(line, separator, 2)

		if len(parts) != 2 && line != delimiter {
			continue
		}

		if line == delimiter {
			if sc.ShouldSleep {
				doSleep(sc.SleepIntervalMs)
				sc = entity.SentinelCommand{}
				continue
			}

			processCommandBlock(ctx, sc)
			sc = entity.SentinelCommand{}
			continue
		}

		key := parts[0]
		value := parts[1]

		switch command(key) {
		case exit:
			// exit.
			log.InfoLn(
				cid,
				"exit found during initialization.",
				"skipping the rest of the commands.",
				"skipping post initialization.",
			)
			// Move out of the loop to allow the keystone secret to be registered.
			break out
		case workload:
			sc.WorkloadIds = strings.SplitN(value, itemSeparator, -1)
		case namespace:
			sc.Namespaces = strings.SplitN(value, itemSeparator, -1)
		case secret:
			sc.Secret = value
		case transformation:
			sc.Template = value
		case sleep:
			sc.ShouldSleep = true
			intms, err := strconv.Atoi(value)
			if err != nil {
				log.ErrorLn(cid, "RunInitCommands: Error parsing sleep interval: ", err.Error())
			}
			sc.SleepIntervalMs = intms
		default:
			log.InfoLn(cid, "RunInitCommands: unknown command: ", key)
		}
	}

	if err := scanner.Err(); err != nil {
		log.ErrorLn(
			cid,
			"RunInitCommands: Error reading initialization file: ",
			err.Error(),
		)
	}

	// Assign a secret for VSecM Keystone
	processCommandBlock(ctx, entity.SentinelCommand{
		WorkloadIds: []string{"vsecm-keystone"},
		Namespaces:  []string{"vsecm-system"},
		Secret:      "keystone-init",
	})

	safe.PostInitializationComplete(ctx)
}
