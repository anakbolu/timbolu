package repository

import (
	"time"

	"github.com/google/uuid"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/repository/sqlcgen"
)

// The conversions from the generated rows to the domain types.
//
// sqlc gives every query its own row type even when two queries select the same
// columns, so the routine rows are read through one shared field set while the run
// rows — which differ in whether they carry a routine link — are written out.
// Reading a run's nullable link through the helper below is what keeps a run whose
// routine was deleted from reading as a routine with the zero uuid.

// routineRow is the column set every routine query returns.
type routineRow struct {
	ID            uuid.UUID
	WorkspaceID   uuid.UUID
	AgentID       uuid.UUID
	Name          string
	Kind          string
	Minute        int32
	HourStart     *int32
	HourEnd       *int32
	DayOfWeek     *int32
	TriggerSource string
	Instruction   string
	Enabled       bool
	PausedReason  string
	ScheduleID    string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func fromCreate(row sqlcgen.CreateRoutineRow) domain.Routine {
	return toRoutine(routineRow(row))
}

func fromGet(row sqlcgen.GetRoutineRow) domain.Routine {
	return toRoutine(routineRow(row))
}

func fromList(row sqlcgen.ListRoutinesRow) domain.Routine {
	return toRoutine(routineRow(row))
}

func fromTrigger(row sqlcgen.ListRoutinesByTriggerRow) domain.Routine {
	return toRoutine(routineRow(row))
}

func fromEnabled(row sqlcgen.ListEnabledRoutinesRow) domain.Routine {
	return toRoutine(routineRow(row))
}

func fromUpdate(row sqlcgen.UpdateRoutineRow) domain.Routine {
	return toRoutine(routineRow(row))
}

// toRoutine renders one stored routine.
//
// A nil hour is read as zero rather than as "no hour": the columns are nullable
// because the bootstrap migration allowed it, and every cadence the product can
// produce states one. The day is the opposite — a nil there is a cadence that does
// not use a weekday, which is what zero means in the domain.
func toRoutine(row routineRow) domain.Routine {
	return domain.Routine{
		ID:            row.ID,
		WorkspaceID:   row.WorkspaceID,
		AgentID:       row.AgentID,
		Name:          row.Name,
		Kind:          row.Kind,
		Minute:        int(row.Minute),
		HourStart:     derefHour(row.HourStart),
		HourEnd:       derefHour(row.HourEnd),
		DayOfWeek:     derefDay(row.DayOfWeek),
		TriggerSource: row.TriggerSource,
		Instruction:   row.Instruction,
		Enabled:       row.Enabled,
		PausedReason:  row.PausedReason,
		ScheduleID:    row.ScheduleID,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
}

// runRow is the column set every run query returns.
type runRow struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	RoutineID   *uuid.UUID
	RoutineName string
	TaskID      *uuid.UUID
	Status      string
	Summary     string
	StartedAt   time.Time
	FinishedAt  *time.Time
}

func fromStartRun(row sqlcgen.StartRoutineRunRow) domain.Run   { return toRun(runRow(row)) }
func fromFinishRun(row sqlcgen.FinishRoutineRunRow) domain.Run { return toRun(runRow(row)) }
func fromGetRun(row sqlcgen.GetRoutineRunRow) domain.Run       { return toRun(runRow(row)) }
func fromLatestRun(row sqlcgen.LatestRoutineRunRow) domain.Run { return toRun(runRow(row)) }
func fromListRun(row sqlcgen.ListRoutineRunsRow) domain.Run    { return toRun(runRow(row)) }
func fromAttachTask(row sqlcgen.AttachRoutineRunTaskRow) domain.Run {
	return toRun(runRow(row))
}
func fromRunByTask(row sqlcgen.GetRoutineRunByTaskRow) domain.Run { return toRun(runRow(row)) }

func toRun(row runRow) domain.Run {
	return domain.Run{
		ID:          row.ID,
		WorkspaceID: row.WorkspaceID,
		RoutineID:   derefID(row.RoutineID),
		RoutineName: row.RoutineName,
		TaskID:      derefID(row.TaskID),
		Status:      row.Status,
		Summary:     row.Summary,
		StartedAt:   row.StartedAt,
		FinishedAt:  row.FinishedAt,
	}
}

func derefID(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func derefHour(value *int32) int {
	if value == nil {
		return 0
	}
	return int(*value)
}

func derefDay(value *int32) int {
	if value == nil {
		return 0
	}
	return int(*value)
}
