package cli

import (
	"context"
	"fmt"
	"strings"
)

func (c *CLI) consumers(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "consumers"}
	}
	if c.opts.Consumers == nil {
		return fmt.Errorf("Consumers are only known in blackbox-api: docker exec -it blackbox-api /blackbox shell")
	}
	list := c.opts.Consumers()
	rows := [][]string{{"CONSUMER", "COVE KEY", "SCOPES", "TOKEN"}}
	for _, k := range list {
		rows = append(rows, []string{k.Name, k.Key, strings.Join(k.Scopes, ", "), k.Token})
	}
	table(rows)
	info(fmt.Sprintf("%d %s", len(list), plural(len(list), "consumer", "consumers")))
	for _, k := range list {
		if k.Token != "present" {
			warn(fmt.Sprintf("%q's token is %s: in Cove, \"generate %s 64\", then redeploy Blackbox.", k.Name, k.Token, k.Key))
		}
	}
	return nil
}

func (c *CLI) migrate(ctx context.Context, args []string) error {
	if len(args) != 2 || args[1] != "status" {
		return usageError{form: "migrate status"}
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	answer, err := client.Migrations(ctx)
	if err != nil {
		return err
	}
	rows := [][]string{{"VERSION", "MIGRATION", "APPLIED"}}
	pending := 0
	for _, m := range answer.Migrations {
		applied := formatTimePtr(m.AppliedAt)
		if m.AppliedAt == nil {
			applied, pending = "pending", pending+1
		}
		rows = append(rows, []string{fmt.Sprint(m.Version), m.Name, applied})
	}
	table(rows)
	info(fmt.Sprintf("%d %s", len(answer.Migrations), plural(len(answer.Migrations), "migration", "migrations")))
	if pending > 0 {
		warn(fmt.Sprintf("%d %s pending: blackbox-collector applies them when it starts; \"docker logs blackbox-collector\" shows why it hasn't.", pending, plural(pending, "migration is", "migrations are")))
	}
	return nil
}
