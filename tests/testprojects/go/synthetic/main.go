// Synthetic test project for MCP-сервер tests
// Tests for: find_implementations, find_references, resolve_call issues

package main

import "fmt"

// Processor — interface for testing implementations detection
type Processor interface {
	Process(data string) string
	Validate() bool
}

// Worker — another interface for testing
type Worker interface {
	Work() error
	Stop()
}

// DataProcessor — implements Processor (Go implicit implementation)
type DataProcessor struct {
	name string
}

func (dp *DataProcessor) Process(data string) string {
	return "processed: " + data
}

func (dp *DataProcessor) Validate() bool {
	return dp.name != ""
}

// StringWorker — implements Worker
type StringWorker struct {
	active bool
}

func (sw *StringWorker) Work() error {
	if !sw.active {
		return fmt.Errorf("worker not active")
	}
	return nil
}

func (sw *StringWorker) Stop() {
	sw.active = false
}

// MultiImpl — implements both Processor and Worker
type MultiImpl struct {
	DataProcessor
	StringWorker
}

// TaskHandler — function that uses interfaces
func TaskHandler(p Processor) string {
	if p.Validate() {
		return p.Process("task")
	}
	return "invalid"
}

// RunWorker — function that uses Worker interface
func RunWorker(w Worker) error {
	return w.Work()
}

// CallProcessor — calls Process method (for reference testing)
func CallProcessor(p Processor) {
	_ = p.Process("test")
}

// CallWorker — calls Work method (for reference testing)
func CallWorker(w Worker) {
	_ = w.Work()
}

// MethodCaller — struct with methods for testing method references
type MethodCaller struct{}

// DoWork — method for testing
func (mc *MethodCaller) DoWork() string {
	return "working"
}

// DoProcess — method that calls other method
func (mc *MethodCaller) DoProcess(data string) string {
	return mc.DoWork() + ": " + data
}

// CallDoWork — function that calls method
func CallDoWork(mc *MethodCaller) string {
	return mc.DoWork()
}

func main() {
	dp := &DataProcessor{name: "test"}
	sw := &StringWorker{active: true}
	mi := &MultiImpl{}

	TaskHandler(dp)
	RunWorker(sw)
	CallProcessor(dp)
	CallWorker(sw)

	mc := &MethodCaller{}
	CallDoWork(mc)
	mc.DoProcess("data")

	fmt.Println("Synthetic test project")
}
