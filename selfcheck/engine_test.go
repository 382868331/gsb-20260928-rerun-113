package selfcheck_test

import (
	"context"
	"testing"
	"time"

	"device-selfcheck/selfcheck"
	"device-selfcheck/virtual"
)

func TestRunDistinguishesFailureKindsAndBlocksDependents(t *testing.T) {
	tests := []struct {
		name       string
		device     virtual.Device
		wantCodes  map[string]string
		wantStatus map[string]selfcheck.Status
	}{
		{
			name: "sensor missing",
			device: virtual.Device{
				ID:            "missing-sensor",
				CommOK:        true,
				HasSensor:     false,
				SensorReading: 0,
				MinReading:    10,
				MaxReading:    20,
			},
			wantCodes: map[string]string{
				virtual.CheckCommLink:       "communication_ok",
				virtual.CheckSensorPresence: "sensor_missing",
				virtual.CheckSensorReading:  "dependency_not_passed",
				virtual.CheckCalibration:    "dependency_not_passed",
				virtual.CheckHealthSummary:  "dependency_not_passed",
			},
			wantStatus: map[string]selfcheck.Status{
				virtual.CheckCommLink:       selfcheck.StatusPassed,
				virtual.CheckSensorPresence: selfcheck.StatusFailed,
				virtual.CheckSensorReading:  selfcheck.StatusBlocked,
				virtual.CheckCalibration:    selfcheck.StatusBlocked,
				virtual.CheckHealthSummary:  selfcheck.StatusBlocked,
			},
		},
		{
			name: "communication works but reading is out of range",
			device: virtual.Device{
				ID:            "bad-reading",
				CommOK:        true,
				HasSensor:     true,
				SensorReading: 42,
				MinReading:    10,
				MaxReading:    20,
			},
			wantCodes: map[string]string{
				virtual.CheckCommLink:       "communication_ok",
				virtual.CheckSensorPresence: "sensor_present",
				virtual.CheckSensorReading:  "sensor_reading_out_of_range",
				virtual.CheckCalibration:    "dependency_not_passed",
				virtual.CheckHealthSummary:  "dependency_not_passed",
			},
			wantStatus: map[string]selfcheck.Status{
				virtual.CheckCommLink:       selfcheck.StatusPassed,
				virtual.CheckSensorPresence: selfcheck.StatusPassed,
				virtual.CheckSensorReading:  selfcheck.StatusFailed,
				virtual.CheckCalibration:    selfcheck.StatusBlocked,
				virtual.CheckHealthSummary:  selfcheck.StatusBlocked,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine, err := selfcheck.NewEngine(virtual.CheckSet())
			if err != nil {
				t.Fatalf("NewEngine() error = %v", err)
			}

			reports, err := engine.Run(context.Background(), tt.device)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if len(reports) != 5 {
				t.Fatalf("len(reports) = %d, want 5", len(reports))
			}

			for checkID, wantCode := range tt.wantCodes {
				report := engine.Store().Latest(checkID)
				if report.Code != wantCode {
					t.Errorf("%s code = %q, want %q", checkID, report.Code, wantCode)
				}
				if report.Status != tt.wantStatus[checkID] {
					t.Errorf("%s status = %q, want %q", checkID, report.Status, tt.wantStatus[checkID])
				}
			}
		})
	}
}

func TestRerunExpiresOnlyRelatedConclusionsAndKeepsHistory(t *testing.T) {
	device := virtual.Device{
		ID:            "repairable",
		CommOK:        true,
		HasSensor:     false,
		SensorReading: 0,
		MinReading:    10,
		MaxReading:    20,
	}
	engine, err := selfcheck.NewEngine(virtual.CheckSet())
	if err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	if _, err := engine.Run(context.Background(), device); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	device.HasSensor = true
	device.SensorReading = 15
	rerunReports, err := engine.Rerun(context.Background(), virtual.CheckSensorPresence, device)
	if err != nil {
		t.Fatalf("Rerun() error = %v", err)
	}

	comm := engine.Store().Latest(virtual.CheckCommLink)
	sensorPresence := engine.Store().Latest(virtual.CheckSensorPresence)
	sensorReading := engine.Store().Latest(virtual.CheckSensorReading)
	calibration := engine.Store().Latest(virtual.CheckCalibration)
	summary := engine.Store().Latest(virtual.CheckHealthSummary)

	if comm.Attempt != 1 {
		t.Errorf("communication attempt = %d, want unchanged 1", comm.Attempt)
	}
	if sensorPresence.Attempt != 2 || sensorPresence.Status != selfcheck.StatusPassed {
		t.Errorf("sensor presence = attempt %d status %s, want attempt 2 passed", sensorPresence.Attempt, sensorPresence.Status)
	}
	if sensorReading.Status != selfcheck.StatusExpired || sensorReading.Code != "upstream_reran" {
		t.Errorf("sensor reading = %s/%s, want expired/upstream_reran", sensorReading.Status, sensorReading.Code)
	}
	if calibration.Status != selfcheck.StatusExpired {
		t.Errorf("calibration status = %s, want expired", calibration.Status)
	}
	if summary.Status != selfcheck.StatusExpired {
		t.Errorf("summary status = %s, want expired", summary.Status)
	}
	if len(rerunReports) != 4 {
		t.Fatalf("len(rerunReports) = %d, want 4", len(rerunReports))
	}
	if len(engine.Store().History()) != 9 {
		t.Fatalf("history length = %d, want 9", len(engine.Store().History()))
	}
}

func TestCancelationAndTimeoutAreRecordedSeparately(t *testing.T) {
	tests := []struct {
		name  string
		setup func() (context.Context, context.CancelFunc)
		want  selfcheck.Status
	}{
		{
			name: "canceled",
			setup: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			want: selfcheck.StatusCanceled,
		},
		{
			name: "timeout",
			setup: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			want: selfcheck.StatusTimedOut,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := tt.setup()
			set := selfcheck.CheckSet[string]{
				Model: "test",
				Checks: []selfcheck.Check[string]{{
					ID:      "slow",
					Timeout: 20 * time.Millisecond,
					Run: func(ctx context.Context, _ string) selfcheck.Result {
						select {
						case <-time.After(time.Second):
							return selfcheck.Passed("ok", "ok", nil)
						case <-ctx.Done():
							return selfcheck.Result{Status: selfcheck.StatusCanceled, Code: "check_canceled", Message: ctx.Err().Error()}
						}
					},
				}},
			}
			engine, err := selfcheck.NewEngine(set)
			if err != nil {
				t.Fatalf("NewEngine() error = %v", err)
			}
			if tt.name == "canceled" {
				go func() {
					time.Sleep(10 * time.Millisecond)
					cancel()
				}()
			}
			defer cancel()

			if _, err := engine.Run(ctx, "device"); err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			report := engine.Store().Latest("slow")
			if report.Status != tt.want {
				t.Errorf("status = %s, want %s", report.Status, tt.want)
			}
		})
	}
}

func TestInvalidDependencyGraph(t *testing.T) {
	_, err := selfcheck.NewEngine(selfcheck.CheckSet[string]{
		Model: "bad",
		Checks: []selfcheck.Check[string]{
			{ID: "a", DependsOn: []string{"b"}},
			{ID: "b", DependsOn: []string{"a"}},
		},
	})
	if err == nil {
		t.Fatalf("NewEngine() error = %v, want non-nil", err)
	}
}

func TestUnmetPreconditionIsBlocked(t *testing.T) {
	set := selfcheck.CheckSet[string]{
		Model: "precondition-test",
		Checks: []selfcheck.Check[string]{{
			ID: "guarded",
			Precondition: func(device string) (bool, string) {
				return false, "device is in maintenance mode"
			},
			Run: func(_ context.Context, _ string) selfcheck.Result {
				t.Fatal("Run must not be called when precondition is unmet")
				return selfcheck.Passed("unexpected", "unexpected", nil)
			},
		}},
	}
	engine, err := selfcheck.NewEngine(set)
	if err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	if _, err := engine.Run(context.Background(), "device"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	report := engine.Store().Latest("guarded")
	if report.Status != selfcheck.StatusBlocked || report.Code != "precondition_not_met" {
		t.Fatalf("report = %s/%s, want blocked/precondition_not_met", report.Status, report.Code)
	}
}
