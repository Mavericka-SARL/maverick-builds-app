//go:build ignore

package main

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
)

func main() {
	ctx := context.Background()
	pool, _ := pgxpool.New(ctx, "postgres://mavericks:mavericks@localhost:5432/mavericks?sslmode=disable")
	defer pool.Close()

	id := os.Args[1]
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM workflow.workflow_instance WHERE id=$1::uuid`, id).Scan(&status)
	fmt.Printf("Instance status: %s\n", status)

	rows, err := pool.Query(ctx, `SELECT id::text, step_def_id, status::text FROM workflow.workflow_step WHERE instance_id=$1::uuid ORDER BY created_at`, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query err: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()
	fmt.Println("Steps:")
	for rows.Next() {
		var sid, defID, sts string
		_ = rows.Scan(&sid, &defID, &sts)
		fmt.Printf("  %-40s  step_def=%-25s  status=%s\n", sid, defID, sts)
	}
}
