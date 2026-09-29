package main

import (
	"context"
	"fmt"

	"device-selfcheck/selfcheck"
	"device-selfcheck/virtual"
)

func main() {
	device := virtual.Device{
		ID:            "sensor-01",
		CommOK:        true,
		HasSensor:     true,
		SensorReading: 42,
		MinReading:    10,
		MaxReading:    20,
	}

	engine, err := selfcheck.NewEngine(virtual.CheckSet())
	if err != nil {
		panic(err)
	}
	reports, err := engine.Run(context.Background(), device)
	if err != nil {
		panic(err)
	}

	for _, report := range reports {
		fmt.Printf("%s: %s (%s) - %s\n", report.CheckID, report.Status, report.Code, report.Message)
	}
}
