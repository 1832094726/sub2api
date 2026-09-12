package repository

import (
	"context"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestCyberUserStrikeTransaction(t *testing.T) {
	for _, tc := range []struct {
		name      string
		role      string
		duplicate bool
		strikes   int
	}{{"first", "user", false, 1}, {"second", "user", false, 2}, {"duplicate", "user", true, 1}, {"admin exempt", "admin", false, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			r := &contentModerationRepository{db: db}
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT role,status FROM users.*FOR UPDATE").WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"role", "status"}).AddRow(tc.role, "active"))
			if tc.role != "admin" {
				affected := int64(1)
				if tc.duplicate {
					affected = 0
				}
				mock.ExpectExec("INSERT INTO downstream_cyber_events").WithArgs(int64(42), "req").WillReturnResult(sqlmock.NewResult(0, affected))
				query := "INSERT INTO downstream_cyber_risk"
				if tc.duplicate {
					query = "SELECT strikes,blocked_until"
				}
				rows := sqlmock.NewRows([]string{"strikes", "blocked_until"})
				if tc.strikes == 1 {
					rows.AddRow(1, time.Now().Add(time.Hour))
				} else {
					rows.AddRow(2, nil)
				}
				mock.ExpectQuery(query).WithArgs(int64(42)).WillReturnRows(rows)
				if tc.strikes == 2 {
					mock.ExpectExec("UPDATE users SET status").WithArgs(int64(42), service.StatusDisabled).WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			mock.ExpectCommit()
			state, err := r.RecordCyberUserStrike(context.Background(), 42, "req")
			require.NoError(t, err)
			require.Equal(t, tc.strikes, state.Strikes)
			require.Equal(t, !tc.duplicate && tc.role != "admin", state.Applied)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
func TestCyberUserStrikeRollsBackOnDisableFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &contentModerationRepository{db: db}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT role,status").WillReturnRows(sqlmock.NewRows([]string{"role", "status"}).AddRow("user", "active"))
	mock.ExpectExec("INSERT INTO downstream_cyber_events").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("INSERT INTO downstream_cyber_risk").WillReturnRows(sqlmock.NewRows([]string{"strikes", "blocked_until"}).AddRow(2, nil))
	mock.ExpectExec("UPDATE users").WillReturnError(context.DeadlineExceeded)
	mock.ExpectRollback()
	_, err = r.RecordCyberUserStrike(context.Background(), 42, "req")
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
