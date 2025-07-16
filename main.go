package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// Структуры
type Task struct {
	RunAfterSeconds int    `json:"run_after_seconds,omitempty"`
	Command         string `json:"command,omitempty"`
}

type Report struct {
	ID     string `json:"id"`
	Task   Task   `json:"task"`
	Status string `json:"status"`
}

type FullReport struct {
	BaseReport Report    `json:"base_report"`
	ExitCode   int       `json:"exit_code"`
	StdOut     string    `json:"stdout"`
	StdErr     error     `json:"stderr"`
	RunAt      time.Time `json:"run_at"`
}

// Ограничитель времени отсрочки выполнения
const timeOutTimer time.Duration = 30 * time.Second

// Ограничитель кол-ва исполняемых тасков
const limitingTasks int = 5

var activeTasks int = 0

// Списки для работы с тасками и историей
var (
	tasks   []FullReport
	history []FullReport

	tasksMu   sync.RWMutex
	historyMu sync.RWMutex
)

// WHITELIST
var allowedCommands = map[string]bool{
	"echo":   true,
	"date":   true,
	"ls":     true,
	"uptime": true,
	"whoami": true,
	"pwd":    true,
	"ping":   true,
}

// Статус ченжеры
func (this *FullReport) changeStatuses(stdout string, stderr error) {

	this.StdOut = stdout
	this.StdErr = stderr

	if stdout == "" {
		this.BaseReport.Status = "scheduled"
	} else {
		this.BaseReport.Status = "executed"
	}

	if stderr == nil {
		this.ExitCode = 0
	} else {
		this.ExitCode = 1
		this.BaseReport.Status = "failed"
	}
}

func (this *FullReport) statusDeleted() {

	this.StdOut = ""
	this.StdErr = nil

	this.ExitCode = 0

	this.BaseReport.Status = "canceled"
}

// Конструктор
func makeFullReport(ID string, task Task, stdout string, stderr error) FullReport {

	fullReport := FullReport{Report{ID, task, ""}, 0, stdout, stderr, time.Now().Add(time.Duration(task.RunAfterSeconds))}

	fullReport.changeStatuses(stdout, stderr)

	return fullReport
}

// Генератор случайных ID
func generateID() string {

	rand.Seed(time.Now().UnixNano())
	chars := []rune("ABCDEFGHIJKLMNOPQRSTUVWXYZ" + "abcdefghijklmnopqrstuvwxyz" + "0123456789")
	length := 8
	var b strings.Builder
	for i := 0; i < length; i++ {
		b.WriteRune(chars[rand.Intn(len(chars))])
	}
	str := b.String()

	if findByID(str) == -1 {
		return str
	}
	return generateID()
}

// Поисковик
func findByID(ID string) int {

	tasksMu.RLock()
	defer tasksMu.RUnlock()

	for i := 0; i < len(tasks); i++ {
		if tasks[i].BaseReport.ID == ID {
			return i
		}
	}

	return -1
}

// Загрузчик
func loadHistory() {

	//Загружаем историю из файла
	data, err := os.ReadFile("history.json")

	if err == nil {
		historyMu.Lock()
		json.Unmarshal(data, &history)
		historyMu.Unlock()
	}
}

// Сохранение в файлы
func saveTasks() error {

	// Превращаем в битовый массив
	tasksMu.RLock()
	updated, err := json.MarshalIndent(tasks, "", "  ")
	tasksMu.RUnlock()

	if err != nil {
		return err
	}

	//Записываем в файл
	return os.WriteFile("tasks.json", updated, 0644)
}

func saveReportToFile(report FullReport) error {

	// Читаем существующий файл, если он есть
	data, err := os.ReadFile("tasks.json")
	if err == nil {
		tasksMu.Lock()
		json.Unmarshal(data, &tasks)
		tasksMu.Unlock()
	}

	// Добавляем новую задачу
	tasksMu.Lock()
	tasks = append(tasks, report)
	tasksMu.Unlock()

	saveReportToHistory(report)

	// Перезаписываем файл
	return saveTasks()
}

func saveReportToHistory(report FullReport) error {

	// Читаем существующий файл, если он есть
	data, err := os.ReadFile("history.json")
	if err == nil {
		json.Unmarshal(data, &history)
	}

	// Добавляем новую задачу
	historyMu.Lock()

	history = append(history, report)

	// Превращаем в битовый массив
	updated, err := json.MarshalIndent(history, "", "  ")

	historyMu.Unlock()

	if err != nil {
		return err
	}

	//Записываем в файл
	return os.WriteFile("history.json", updated, 0644)
}

// Валидаторы
func isWhitelisted(cmd string) bool {
	return allowedCommands[cmd]
}

func isCommandValid(command string) bool {

	//Разделение команды на части
	parts := strings.Fields(command)

	//Проверка на отсутствие команды
	if len(parts) == 0 {
		return false
	}

	//Получение самой команды
	cmdName := parts[0]

	//Проверка команды на валидность
	if !isWhitelisted(cmdName) {
		return false
	}

	return true
}

func isTimeValid(timeDelay int) bool {
	return timeDelay >= 1
}

func isReportValid(report FullReport) (bool, error) {

	if !isCommandValid(report.BaseReport.Task.Command) {
		return false, fmt.Errorf("Command is not valid")
	}
	if !isTimeValid(report.BaseReport.Task.RunAfterSeconds) {
		return false, fmt.Errorf("Time is not valid")
	}
	return true, nil
}

// Функции производящие работу с задачами
func executeComand(report FullReport) {

	cmd := exec.Command("powershell", "-Command", report.BaseReport.Task.Command)

	output, err := cmd.CombinedOutput()
	if err != nil {
		report.changeStatuses("", err)
	} else {
		report.changeStatuses(string(output), err)
	}

	activeTasks--

	deleteFromTasks(findByID(report.BaseReport.ID))
	saveReportToFile(report)
}

func delayTask(report FullReport, w http.ResponseWriter) {

	ctx, cancel := context.WithTimeout(context.Background(), timeOutTimer)

	activeTasks++

	// При выходе из функции закрыть канал контекста
	defer cancel()

	select {
	// Если ограничение не сработало, исполнение таска
	case <-time.After(time.Duration(report.BaseReport.Task.RunAfterSeconds) * time.Second):

		// Проверка на отмену задачи
		if tasks[findByID(report.BaseReport.ID)].BaseReport.Status == "canceled" {
			return
		}

		// Выполнение задачи
		executeComand(report)

	// Ограничение по времени
	case <-ctx.Done():
		report.statusDeleted()
		deleteFromTasks(findByID(report.BaseReport.ID))
		saveReportToFile(report)
		fmt.Fprintf(w, "Task: '%s' canceled due to timeout", report.BaseReport.ID)
	}
}

func collectTask(w http.ResponseWriter, r *http.Request) {

	// Проверка на кол-во задач
	if activeTasks >= limitingTasks {
		http.Error(w, "Tasks overflow", http.StatusBadRequest)
		return
	}

	// Инициализация инструментов и их подготовка
	var task Task
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	// Декодировка JSON'а и проверка на ошибки
	err := decoder.Decode(&task)
	if err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		tasksMu.Lock()
		tasks = append(tasks, makeFullReport(generateID(), Task{0, ""}, "", err))
		tasksMu.Unlock()
		return
	}

	// Сформировать полный запрос
	report := makeFullReport(generateID(), task, "", nil)

	// Проверка на валидность задачи
	statment, err := isReportValid(report)
	if !statment {
		fmt.Fprintf(w, "%v", err)
		return
	}

	// Вывод информации о запросе
	fmt.Fprintf(w, "Задача получена: '%s' \nБудет исполнена через: '%d' секунд \nID задачи: '%s'", report.BaseReport.Task.Command, report.BaseReport.Task.RunAfterSeconds, report.BaseReport.ID)

	// Сохранить запрос
	saveReportToFile(report)

	// Произвести работу с запросом
	go delayTask(report, w)
}

// Функции показа задач/истории
func showTasks(w http.ResponseWriter, r *http.Request) {

	tasksMu.RLock()
	for i := 0; i < len(tasks); i++ {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tasks[i])
	}
	tasksMu.RUnlock()
}

func showHistory(w http.ResponseWriter, r *http.Request) {

	for i := 0; i < len(history); i++ {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(history[i])
	}
}

func showTaskByID(w http.ResponseWriter, r *http.Request) {

	idStr := r.PathValue("id")

	index := findByID(idStr)

	if index == -1 {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	report := tasks[index]

	w.Header().Set("Content-Type", "application/json")
	tasksMu.RLock()
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.Encode(report)
	tasksMu.RUnlock()
}

// Функции удаления задач
func deleteFromTasks(deletingIndex int) {

	tasksMu.Lock()
	tasks = slices.Delete(tasks, deletingIndex, deletingIndex+1)
	tasksMu.Unlock()

	saveTasks()
}

func deleteTask(w http.ResponseWriter, r *http.Request) {

	idStr := r.PathValue("id")

	deletingIndex := findByID(idStr)

	reportStatusDeleted := tasks[deletingIndex]
	reportStatusDeleted.statusDeleted()

	if tasks[deletingIndex].BaseReport.Status == "executed" {
		fmt.Fprintf(w, "410")
	} else {
		deleteFromTasks(deletingIndex)
		saveReportToFile(reportStatusDeleted)
		fmt.Fprintf(w, "400")
	}
}

// Запуск сервера
func main() {

	loadHistory()

	mux := http.NewServeMux()

	mux.HandleFunc("POST /tasks", collectTask)
	mux.HandleFunc("GET /tasks", showTasks)
	mux.HandleFunc("GET /tasks/{id}", showTaskByID)
	mux.HandleFunc("GET /history", showHistory)
	mux.HandleFunc("DELETE /tasks/{id}", deleteTask)

	fmt.Println("Server is running on :8080")
	http.ListenAndServe(":8080", mux)
}
