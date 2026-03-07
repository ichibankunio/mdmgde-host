package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

type jobType string

const (
	jobTypeRun     jobType = "run"
	jobTypeImprove jobType = "improve"
	jobTypeMerge   jobType = "merge"
	jobTypeDiscard jobType = "discard"

	defaultRunScript     = "scripts/chatops_run_task.sh"
	defaultMergeScript   = "scripts/chatops_merge_branch.sh"
	defaultDiscardScript = "scripts/chatops_discard_branch.sh"
)

type config struct {
	BotToken      string
	AppID         string
	GuildID       string
	WorkDir       string
	AllowedRepos  map[string]struct{}
	RunScript     string
	MergeScript   string
	DiscardScript string
	PreviewURLTpl string
	MaxLogLines   int
	QueueCapacity int
}

type job struct {
	ID          string
	Type        jobType
	Repo        string
	Task        string
	Branch      string
	ThreadName  string
	RequestedBy string
	ChannelID   string
	StatusMsgID string
	Question    string
	RequestedAt time.Time
}

type jobStatus string

const (
	statusQueued    jobStatus = "queued"
	statusRunning   jobStatus = "running"
	statusWaiting   jobStatus = "waiting_input"
	statusSucceeded jobStatus = "succeeded"
	statusFailed    jobStatus = "failed"
)

type jobResult struct {
	Status     string `json:"status"`
	Branch     string `json:"branch"`
	PreviewURL string `json:"preview_url"`
	Summary    string `json:"summary"`
	LogFile    string `json:"log_file"`
	Message    string `json:"message"`
}

type jobRecord struct {
	Job       job
	Status    jobStatus
	UpdatedAt time.Time
	Result    jobResult
	Err       string
}

type server struct {
	cfg config
	dg  *discordgo.Session

	queue chan job

	mu      sync.Mutex
	records map[string]*jobRecord
	running string
	uiState map[string]uiState
	threads map[string]threadContext
}

type uiState struct {
	Action      string
	Repo        string
	Branch      string
	RequestedBy string
	ChannelID   string
	JobID       string
	Task        string
	Question    string
}

type threadContext struct {
	Repo   string
	Branch string
}

type lineCapture struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	pending string
	onLine  func(string)
}

func (w *lineCapture) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.buf.Write(p)
	data := w.pending + string(p)
	parts := strings.Split(data, "\n")
	for i := 0; i < len(parts)-1; i++ {
		line := strings.TrimRight(parts[i], "\r")
		if w.onLine != nil {
			w.onLine(line)
		}
	}
	w.pending = parts[len(parts)-1]
	return len(p), nil
}

func (w *lineCapture) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending != "" && w.onLine != nil {
		w.onLine(strings.TrimRight(w.pending, "\r"))
	}
	w.pending = ""
}

func (w *lineCapture) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]byte, w.buf.Len())
	copy(out, w.buf.Bytes())
	return out
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	dg, err := discordgo.New("Bot " + cfg.BotToken)
	if err != nil {
		log.Fatalf("failed to create Discord session: %v", err)
	}

	s := &server{
		cfg:     cfg,
		dg:      dg,
		queue:   make(chan job, cfg.QueueCapacity),
		records: make(map[string]*jobRecord),
		uiState: make(map[string]uiState),
		threads: make(map[string]threadContext),
	}

	dg.AddHandler(s.onReady)
	dg.AddHandler(s.onInteractionCreate)
	dg.AddHandler(s.onMessageCreate)
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent

	if err := dg.Open(); err != nil {
		log.Fatalf("failed to open Discord connection: %v", err)
	}
	defer dg.Close()

	go s.workerLoop()

	log.Printf("discord-worker started (guild=%q)", cfg.GuildID)
	select {}
}

func loadConfig() (config, error) {
	botToken := strings.TrimSpace(os.Getenv("DISCORD_BOT_TOKEN"))
	if botToken == "" {
		return config{}, errors.New("DISCORD_BOT_TOKEN is required")
	}
	appID := strings.TrimSpace(os.Getenv("DISCORD_APP_ID"))
	if appID == "" {
		return config{}, errors.New("DISCORD_APP_ID is required")
	}
	guildID := strings.TrimSpace(os.Getenv("DISCORD_GUILD_ID"))
	workDir := strings.TrimSpace(os.Getenv("CHATOPS_WORKDIR"))
	if workDir == "" {
		workDir = "."
	}
	runScript := strings.TrimSpace(os.Getenv("CHATOPS_RUN_SCRIPT"))
	if runScript == "" {
		runScript = defaultRunScript
	}
	mergeScript := strings.TrimSpace(os.Getenv("CHATOPS_MERGE_SCRIPT"))
	if mergeScript == "" {
		mergeScript = defaultMergeScript
	}
	discardScript := strings.TrimSpace(os.Getenv("CHATOPS_DISCARD_SCRIPT"))
	if discardScript == "" {
		discardScript = defaultDiscardScript
	}
	previewTpl := strings.TrimSpace(os.Getenv("CHATOPS_PREVIEW_URL_TEMPLATE"))
	maxLogLines := parseIntEnv("CHATOPS_MAX_LOG_LINES", 120)
	queueCapacity := parseIntEnv("CHATOPS_QUEUE_CAPACITY", 64)

	allowed := make(map[string]struct{})
	for _, repo := range strings.Split(os.Getenv("CHATOPS_ALLOWED_REPOS"), ",") {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		allowed[repo] = struct{}{}
	}
	if len(allowed) == 0 {
		return config{}, errors.New("CHATOPS_ALLOWED_REPOS is required (comma-separated owner/repo)")
	}

	return config{
		BotToken:      botToken,
		AppID:         appID,
		GuildID:       guildID,
		WorkDir:       workDir,
		AllowedRepos:  allowed,
		RunScript:     runScript,
		MergeScript:   mergeScript,
		DiscardScript: discardScript,
		PreviewURLTpl: previewTpl,
		MaxLogLines:   maxLogLines,
		QueueCapacity: queueCapacity,
	}, nil
}

func parseIntEnv(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func (s *server) onReady(_ *discordgo.Session, _ *discordgo.Ready) {
	if err := s.registerCommands(); err != nil {
		log.Printf("failed to register commands: %v", err)
		return
	}
	log.Printf("slash commands registered")
}

func (s *server) registerCommands() error {
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "start",
			Description: "Open mobile-friendly ChatOps actions",
		},
		{
			Name:        "run",
			Description: "Queue a Codex development task",
			Options: []*discordgo.ApplicationCommandOption{
				{Name: "repo", Description: "owner/repo", Type: discordgo.ApplicationCommandOptionString, Required: false},
				{Name: "task", Description: "Task for Codex", Type: discordgo.ApplicationCommandOptionString, Required: false},
			},
		},
		{
			Name:        "status",
			Description: "Show queue/running job status",
			Options: []*discordgo.ApplicationCommandOption{
				{Name: "job_id", Description: "Specific job ID", Type: discordgo.ApplicationCommandOptionString, Required: false},
			},
		},
		{
			Name:        "logs",
			Description: "Show recent lines from a job log",
			Options: []*discordgo.ApplicationCommandOption{
				{Name: "job_id", Description: "Job ID", Type: discordgo.ApplicationCommandOptionString, Required: true},
				{Name: "lines", Description: "Line count", Type: discordgo.ApplicationCommandOptionInteger, Required: false},
			},
		},
		{
			Name:        "improve",
			Description: "Queue an additional change on an existing task branch",
			Options: []*discordgo.ApplicationCommandOption{
				{Name: "repo", Description: "owner/repo", Type: discordgo.ApplicationCommandOptionString, Required: false},
				{Name: "branch", Description: "task/<...>", Type: discordgo.ApplicationCommandOptionString, Required: false},
				{Name: "task", Description: "Additional task for Codex", Type: discordgo.ApplicationCommandOptionString, Required: false},
			},
		},
		{
			Name:        "merge",
			Description: "Merge a task branch into main",
			Options: []*discordgo.ApplicationCommandOption{
				{Name: "branch", Description: "task/<...>", Type: discordgo.ApplicationCommandOptionString, Required: false},
			},
		},
		{
			Name:        "discard",
			Description: "Abandon a task branch and return to main",
			Options: []*discordgo.ApplicationCommandOption{
				{Name: "branch", Description: "task/<...>", Type: discordgo.ApplicationCommandOptionString, Required: false},
			},
		},
		{
			Name:        "preview",
			Description: "Build preview URL for a branch",
			Options: []*discordgo.ApplicationCommandOption{
				{Name: "branch", Description: "task/<...>", Type: discordgo.ApplicationCommandOptionString, Required: false},
			},
		},
	}

	_, err := s.dg.ApplicationCommandBulkOverwrite(s.cfg.AppID, s.cfg.GuildID, commands)
	return err
}

func (s *server) onInteractionCreate(_ *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		data := i.ApplicationCommandData()
		switch data.Name {
		case "start":
			s.handleStart(i)
		case "run":
			s.handleRun(i, data)
		case "status":
			s.handleStatus(i, data)
		case "logs":
			s.handleLogs(i, data)
		case "improve":
			s.handleImprove(i, data)
		case "merge":
			s.handleMerge(i, data)
		case "discard":
			s.handleDiscard(i, data)
		case "preview":
			s.handlePreview(i, data)
		}
	case discordgo.InteractionMessageComponent:
		s.handleComponent(i)
	case discordgo.InteractionModalSubmit:
		s.handleModalSubmit(i)
	}
}

func (s *server) handleStart(i *discordgo.InteractionCreate) {
	s.respondWithComponents(i, "Choose an action:", true, []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{CustomID: "ui:start:run", Label: "New Run", Style: discordgo.PrimaryButton},
			discordgo.Button{CustomID: "ui:start:improve", Label: "Improve", Style: discordgo.SecondaryButton},
			discordgo.Button{CustomID: "ui:start:merge", Label: "Merge", Style: discordgo.SuccessButton},
			discordgo.Button{CustomID: "ui:start:discard", Label: "Discard", Style: discordgo.DangerButton},
		}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{CustomID: "ui:start:status", Label: "Status", Style: discordgo.SecondaryButton},
			discordgo.Button{CustomID: "ui:start:logs", Label: "Logs", Style: discordgo.SecondaryButton},
		}},
	})
}

func (s *server) handleRun(i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	repo := strings.TrimSpace(optionString(data.Options, "repo"))
	task := strings.TrimSpace(optionString(data.Options, "task"))
	if repo == "" || task == "" {
		s.openRunRepoPicker(i)
		return
	}
	j, err := s.newRunJob(repo, task, requesterID(i), i.ChannelID, true)
	if err != nil {
		s.respond(i, err.Error(), true)
		return
	}
	s.enqueueJob(j)
	s.respond(i, fmt.Sprintf("queued job `%s` for repo `%s` (thread: <#%s>)", j.ID, repo, j.ChannelID), true)
}

func (s *server) handleImprove(i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	repo := strings.TrimSpace(optionString(data.Options, "repo"))
	branch := strings.TrimSpace(optionString(data.Options, "branch"))
	task := strings.TrimSpace(optionString(data.Options, "task"))
	if repo == "" || branch == "" || task == "" {
		s.openBranchPicker(i, "improve", "")
		return
	}
	j, err := s.newImproveJob(repo, branch, task, requesterID(i), i.ChannelID, true)
	if err != nil {
		s.respond(i, err.Error(), true)
		return
	}
	s.enqueueJob(j)
	s.respond(i, fmt.Sprintf("queued improve job `%s` on `%s` (thread: <#%s>)", j.ID, branch, j.ChannelID), true)
}

func (s *server) handleMerge(i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	branch := strings.TrimSpace(optionString(data.Options, "branch"))
	if branch == "" {
		s.openBranchPicker(i, "merge", "")
		return
	}
	s.askBranchConfirm(i, "merge", branch)
}

func (s *server) handlePreview(i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	branch := strings.TrimSpace(optionString(data.Options, "branch"))
	if branch == "" {
		s.openBranchPicker(i, "preview", "")
		return
	}
	if s.cfg.PreviewURLTpl == "" {
		s.respond(i, "CHATOPS_PREVIEW_URL_TEMPLATE is not configured", true)
		return
	}
	url := strings.ReplaceAll(s.cfg.PreviewURLTpl, "{branch}", branch)
	url = strings.ReplaceAll(url, "{branch_slug}", sanitizeBranch(branch))
	s.respond(i, fmt.Sprintf("preview: %s", url), true)
}

func (s *server) handleDiscard(i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	branch := strings.TrimSpace(optionString(data.Options, "branch"))
	if branch == "" {
		s.openBranchPicker(i, "discard", "")
		return
	}
	s.askBranchConfirm(i, "discard", branch)
}

func (s *server) newRunJob(repo, task, requestedBy, channelID string, createThread bool) (job, error) {
	if _, ok := s.cfg.AllowedRepos[repo]; !ok {
		return job{}, fmt.Errorf("repo `%s` is not in CHATOPS_ALLOWED_REPOS", repo)
	}
	jobID := fmt.Sprintf("%s-%d", time.Now().Format("20060102-150405"), time.Now().UnixNano()%10000)
	j := job{
		ID:          jobID,
		Type:        jobTypeRun,
		Repo:        repo,
		Task:        task,
		RequestedBy: requestedBy,
		ChannelID:   channelID,
		RequestedAt: time.Now(),
	}
	if createThread {
		j.ThreadName = fmt.Sprintf("run-%s", jobID)
		threadID := s.startJobThread(channelID, requestedBy, j.ThreadName, fmt.Sprintf("Run started: `%s`\nrequest: %s", jobID, task))
		j.ChannelID = threadID
	}
	return j, nil
}

func (s *server) newImproveJob(repo, branch, task, requestedBy, channelID string, createThread bool) (job, error) {
	if _, ok := s.cfg.AllowedRepos[repo]; !ok {
		return job{}, fmt.Errorf("repo `%s` is not in CHATOPS_ALLOWED_REPOS", repo)
	}
	if !strings.HasPrefix(branch, "task/") {
		return job{}, errors.New("branch must start with `task/`")
	}
	jobID := fmt.Sprintf("improve-%s-%d", time.Now().Format("20060102-150405"), time.Now().UnixNano()%10000)
	j := job{
		ID:          jobID,
		Type:        jobTypeImprove,
		Repo:        repo,
		Task:        task,
		Branch:      branch,
		RequestedBy: requestedBy,
		ChannelID:   channelID,
		RequestedAt: time.Now(),
	}
	if createThread {
		j.ThreadName = fmt.Sprintf("improve-%s", sanitizeBranch(branch))
		threadID := s.startJobThread(channelID, requestedBy, j.ThreadName, fmt.Sprintf("Improve started: `%s` on `%s`\nrequest: %s", jobID, branch, task))
		j.ChannelID = threadID
	}
	return j, nil
}

func (s *server) handleStatus(i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	jobID := strings.TrimSpace(optionString(data.Options, "job_id"))
	s.mu.Lock()
	defer s.mu.Unlock()

	if jobID != "" {
		rec, ok := s.records[jobID]
		if !ok {
			s.respond(i, fmt.Sprintf("job `%s` not found", jobID), true)
			return
		}
		s.respond(i, renderRecord(*rec), true)
		return
	}

	lines := []string{
		fmt.Sprintf("running: %s", fallback(s.running, "none")),
		fmt.Sprintf("queued: %d", len(s.queue)),
		"recent:",
	}
	recent := s.sortedRecordsLocked(8)
	if len(recent) == 0 {
		lines = append(lines, "- no jobs yet")
	} else {
		for _, rec := range recent {
			lines = append(lines, fmt.Sprintf("- %s [%s] %s", rec.Job.ID, rec.Job.Type, rec.Status))
		}
	}
	s.respond(i, strings.Join(lines, "\n"), true)
}

func (s *server) handleLogs(i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	jobID := strings.TrimSpace(optionString(data.Options, "job_id"))
	if jobID == "" {
		s.respond(i, "job_id is required", true)
		return
	}
	lines := optionInt(data.Options, "lines", s.cfg.MaxLogLines)
	if lines > s.cfg.MaxLogLines {
		lines = s.cfg.MaxLogLines
	}

	s.mu.Lock()
	rec, ok := s.records[jobID]
	s.mu.Unlock()
	if !ok {
		s.respond(i, fmt.Sprintf("job `%s` not found", jobID), true)
		return
	}
	if rec.Result.LogFile == "" {
		s.respond(i, "no log file available yet", true)
		return
	}

	logText, err := tailFile(rec.Result.LogFile, lines)
	if err != nil {
		s.respond(i, fmt.Sprintf("failed to read log: %v", err), true)
		return
	}
	if len(logText) > 1800 {
		logText = logText[len(logText)-1800:]
	}
	s.respond(i, fmt.Sprintf("`%s`\n```\n%s\n```", rec.Result.LogFile, logText), true)
}

func (s *server) handleComponent(i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()
	switch {
	case data.CustomID == "ui:start:run":
		s.openRunRepoPicker(i)
	case data.CustomID == "ui:start:improve":
		s.openBranchPicker(i, "improve", "")
	case data.CustomID == "ui:start:merge":
		s.openBranchPicker(i, "merge", "")
	case data.CustomID == "ui:start:discard":
		s.openBranchPicker(i, "discard", "")
	case data.CustomID == "ui:start:status":
		s.respond(i, "Use `/status` or `/status job_id:<id>`.", true)
	case data.CustomID == "ui:start:logs":
		s.respond(i, "Use `/logs job_id:<id>`.", true)
	case strings.HasPrefix(data.CustomID, "ui:repo:"):
		s.onRepoSelected(i, strings.TrimPrefix(data.CustomID, "ui:repo:"))
	case strings.HasPrefix(data.CustomID, "ui:branch:"):
		s.onBranchSelected(i, strings.TrimPrefix(data.CustomID, "ui:branch:"))
	case strings.HasPrefix(data.CustomID, "ui:confirm:"):
		s.onConfirmSelected(i, strings.TrimPrefix(data.CustomID, "ui:confirm:"))
	case strings.HasPrefix(data.CustomID, "ui:quick:"):
		s.onQuickAction(i, strings.TrimPrefix(data.CustomID, "ui:quick:"))
	case strings.HasPrefix(data.CustomID, "ui:question:"):
		s.onQuestionAction(i, strings.TrimPrefix(data.CustomID, "ui:question:"))
	default:
		s.respond(i, "Unknown action.", true)
	}
}

func (s *server) handleModalSubmit(i *discordgo.InteractionCreate) {
	data := i.ModalSubmitData()
	switch {
	case strings.HasPrefix(data.CustomID, "ui:modal:run:"):
		token := strings.TrimPrefix(data.CustomID, "ui:modal:run:")
		st, ok := s.takeUIState(token)
		if !ok {
			s.respond(i, "Session expired. Please run `/start` again.", true)
			return
		}
		task := modalInputValue(data.Components, "task")
		j, err := s.newRunJob(st.Repo, task, requesterID(i), st.ChannelID, true)
		if err != nil {
			s.respond(i, err.Error(), true)
			return
		}
		s.enqueueJob(j)
		s.respond(i, fmt.Sprintf("queued job `%s` for repo `%s` (thread: <#%s>)", j.ID, j.Repo, j.ChannelID), true)
	case strings.HasPrefix(data.CustomID, "ui:modal:improve:"):
		token := strings.TrimPrefix(data.CustomID, "ui:modal:improve:")
		st, ok := s.takeUIState(token)
		if !ok {
			s.respond(i, "Session expired. Please run `/start` again.", true)
			return
		}
		task := modalInputValue(data.Components, "task")
		j, err := s.newImproveJob(st.Repo, st.Branch, task, requesterID(i), st.ChannelID, true)
		if err != nil {
			s.respond(i, err.Error(), true)
			return
		}
		s.enqueueJob(j)
		s.respond(i, fmt.Sprintf("queued improve job `%s` on `%s` (thread: <#%s>)", j.ID, j.Branch, j.ChannelID), true)
	case strings.HasPrefix(data.CustomID, "ui:modal:answer:"):
		token := strings.TrimPrefix(data.CustomID, "ui:modal:answer:")
		st, ok := s.takeUIState(token)
		if !ok {
			s.respond(i, "Session expired. Please run `/start` again.", true)
			return
		}
		answer := modalInputValue(data.Components, "answer")
		if answer == "" {
			s.respond(i, "answer is required", true)
			return
		}
		combinedTask := combineTaskAndAnswer(st.Task, st.Question, answer)
		if st.Branch != "" {
			j, err := s.newImproveJob(st.Repo, st.Branch, combinedTask, requesterID(i), st.ChannelID, false)
			if err != nil {
				s.respond(i, err.Error(), true)
				return
			}
			s.enqueueJob(j)
			s.respond(i, fmt.Sprintf("queued follow-up improve `%s` on `%s`", j.ID, j.Branch), true)
			return
		}
		j, err := s.newRunJob(st.Repo, combinedTask, requesterID(i), st.ChannelID, false)
		if err != nil {
			s.respond(i, err.Error(), true)
			return
		}
		s.enqueueJob(j)
		s.respond(i, fmt.Sprintf("queued follow-up run `%s`", j.ID), true)
	default:
		s.respond(i, "Unknown modal action.", true)
	}
}

func (s *server) onMessageCreate(_ *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}
	if !containsMention(m, s.cfg.AppID) {
		return
	}
	ctx, ok := s.getThreadContext(m.ChannelID)
	if !ok || ctx.Branch == "" || ctx.Repo == "" {
		return
	}
	task := stripMentionsAndTrim(m.Content)
	if task == "" {
		return
	}
	j, err := s.newImproveJob(ctx.Repo, ctx.Branch, task, m.Author.ID, m.ChannelID, false)
	if err != nil {
		_, _ = s.dg.ChannelMessageSend(m.ChannelID, fmt.Sprintf("<@%s> %v", m.Author.ID, err))
		return
	}
	s.enqueueJob(j)
	_, _ = s.dg.ChannelMessageSend(m.ChannelID, fmt.Sprintf("<@%s> queued improve `%s` on `%s`", m.Author.ID, j.ID, j.Branch))
}

func (s *server) openRunRepoPicker(i *discordgo.InteractionCreate) {
	repos := s.allowedRepoList()
	options := make([]discordgo.SelectMenuOption, 0, len(repos))
	for _, r := range repos {
		options = append(options, discordgo.SelectMenuOption{Label: r, Value: r})
	}
	token := s.putUIState(uiState{
		Action:      "run_repo",
		RequestedBy: requesterID(i),
		ChannelID:   i.ChannelID,
	})
	s.respondWithComponents(i, "Select repo:", true, []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{
				CustomID:    "ui:repo:" + token,
				Placeholder: "Choose repo",
				Options:     options,
			},
		}},
	})
}

func (s *server) openBranchPicker(i *discordgo.InteractionCreate, action, repo string) {
	branches, err := s.listTaskBranches()
	if err != nil {
		s.respond(i, fmt.Sprintf("failed to list branches: %v", err), true)
		return
	}
	if len(branches) == 0 {
		s.respond(i, "No task branches found.", true)
		return
	}
	options := make([]discordgo.SelectMenuOption, 0, len(branches))
	for _, b := range branches {
		options = append(options, discordgo.SelectMenuOption{Label: b, Value: b})
	}
	token := s.putUIState(uiState{
		Action:      action,
		Repo:        repo,
		RequestedBy: requesterID(i),
		ChannelID:   i.ChannelID,
	})
	s.respondWithComponents(i, "Select task branch:", true, []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{
				CustomID:    "ui:branch:" + token,
				Placeholder: "Choose branch",
				Options:     options,
			},
		}},
	})
}

func (s *server) onRepoSelected(i *discordgo.InteractionCreate, token string) {
	st, ok := s.takeUIState(token)
	if !ok {
		s.respond(i, "Session expired. Please run `/start` again.", true)
		return
	}
	data := i.MessageComponentData()
	if len(data.Values) == 0 {
		s.respond(i, "No repo selected.", true)
		return
	}
	repo := data.Values[0]
	if st.Action != "run_repo" {
		s.respond(i, "Unexpected repo selection.", true)
		return
	}
	next := s.putUIState(uiState{
		Action:      "run_task",
		Repo:        repo,
		RequestedBy: st.RequestedBy,
		ChannelID:   st.ChannelID,
	})
	s.respondModal(i, "ui:modal:run:"+next, "New Run Task", "task", "Enter task for Codex")
}

func (s *server) onBranchSelected(i *discordgo.InteractionCreate, token string) {
	st, ok := s.takeUIState(token)
	if !ok {
		s.respond(i, "Session expired. Please run `/start` again.", true)
		return
	}
	data := i.MessageComponentData()
	if len(data.Values) == 0 {
		s.respond(i, "No branch selected.", true)
		return
	}
	branch := data.Values[0]
	switch st.Action {
	case "preview":
		url := s.previewURL(branch)
		s.respond(i, fmt.Sprintf("preview: %s", url), true)
	case "merge", "discard":
		s.askBranchConfirm(i, st.Action, branch)
	case "improve":
		repo := st.Repo
		if repo == "" {
			repo = s.defaultRepo()
		}
		next := s.putUIState(uiState{
			Action:      "improve_task",
			Repo:        repo,
			Branch:      branch,
			RequestedBy: st.RequestedBy,
			ChannelID:   st.ChannelID,
		})
		s.respondModal(i, "ui:modal:improve:"+next, "Improve Task", "task", "Enter additional change request")
	default:
		s.respond(i, "Unexpected branch selection.", true)
	}
}

func (s *server) askBranchConfirm(i *discordgo.InteractionCreate, action, branch string) {
	if !strings.HasPrefix(branch, "task/") {
		s.respond(i, "branch must start with `task/`", true)
		return
	}
	token := s.putUIState(uiState{
		Action:      action,
		Branch:      branch,
		RequestedBy: requesterID(i),
		ChannelID:   i.ChannelID,
	})
	s.respondWithComponents(i, fmt.Sprintf("Confirm `%s` for `%s`?", action, branch), true, []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{CustomID: "ui:confirm:" + token + ":yes", Label: "Confirm", Style: discordgo.DangerButton},
			discordgo.Button{CustomID: "ui:confirm:" + token + ":no", Label: "Cancel", Style: discordgo.SecondaryButton},
		}},
	})
}

func (s *server) onConfirmSelected(i *discordgo.InteractionCreate, raw string) {
	parts := strings.Split(raw, ":")
	if len(parts) != 2 {
		s.respond(i, "invalid confirm payload", true)
		return
	}
	token, choice := parts[0], parts[1]
	st, ok := s.takeUIState(token)
	if !ok {
		s.respond(i, "Session expired. Please run `/start` again.", true)
		return
	}
	if choice != "yes" {
		s.respond(i, "Cancelled.", true)
		return
	}
	switch st.Action {
	case "merge":
		j := job{
			ID:          fmt.Sprintf("merge-%s-%d", time.Now().Format("20060102-150405"), time.Now().UnixNano()%10000),
			Type:        jobTypeMerge,
			Branch:      st.Branch,
			RequestedBy: requesterID(i),
			ChannelID:   i.ChannelID,
			RequestedAt: time.Now(),
		}
		s.enqueueJob(j)
		s.respond(i, fmt.Sprintf("queued merge job `%s` for branch `%s`", j.ID, j.Branch), true)
	case "discard":
		j := job{
			ID:          fmt.Sprintf("discard-%s-%d", time.Now().Format("20060102-150405"), time.Now().UnixNano()%10000),
			Type:        jobTypeDiscard,
			Branch:      st.Branch,
			RequestedBy: requesterID(i),
			ChannelID:   i.ChannelID,
			RequestedAt: time.Now(),
		}
		s.enqueueJob(j)
		s.respond(i, fmt.Sprintf("queued discard job `%s` for branch `%s`", j.ID, j.Branch), true)
	default:
		s.respond(i, "Unsupported confirm action.", true)
	}
}

func (s *server) onQuickAction(i *discordgo.InteractionCreate, token string) {
	st, ok := s.takeUIState(token)
	if !ok {
		s.respond(i, "Session expired. Use `/start` again.", true)
		return
	}
	switch st.Action {
	case "quick_improve":
		next := s.putUIState(uiState{
			Action:      "improve_task",
			Repo:        st.Repo,
			Branch:      st.Branch,
			RequestedBy: requesterID(i),
			ChannelID:   i.ChannelID,
		})
		s.respondModal(i, "ui:modal:improve:"+next, "Improve Task", "task", "Enter additional change request")
	case "quick_merge":
		s.askBranchConfirm(i, "merge", st.Branch)
	case "quick_discard":
		s.askBranchConfirm(i, "discard", st.Branch)
	case "quick_retry":
		if st.Repo == "" || st.Task == "" {
			s.respond(i, "cannot retry: missing repo/task", true)
			return
		}
		if st.Branch != "" {
			j, err := s.newImproveJob(st.Repo, st.Branch, st.Task, requesterID(i), i.ChannelID, false)
			if err != nil {
				s.respond(i, err.Error(), true)
				return
			}
			s.enqueueJob(j)
			s.respond(i, fmt.Sprintf("queued retry improve job `%s` on `%s`", j.ID, j.Branch), true)
			return
		}
		j, err := s.newRunJob(st.Repo, st.Task, requesterID(i), i.ChannelID, false)
		if err != nil {
			s.respond(i, err.Error(), true)
			return
		}
		s.enqueueJob(j)
		s.respond(i, fmt.Sprintf("queued retry run job `%s`", j.ID), true)
	case "quick_logs":
		s.respond(i, fmt.Sprintf("Use `/logs job_id:%s`", st.JobID), true)
	default:
		s.respond(i, "Unknown quick action", true)
	}
}

func (s *server) onQuestionAction(i *discordgo.InteractionCreate, token string) {
	st, ok := s.takeUIState(token)
	if !ok {
		s.respond(i, "Session expired. Please run `/start` again.", true)
		return
	}
	next := s.putUIState(st)
	s.respondAnswerModal(i, "ui:modal:answer:"+next, "Answer Codex Question")
}

func (s *server) enqueueJob(j job) {
	j.StatusMsgID = s.createStatusMessage(j)
	s.trackQueued(j)
	s.queue <- j
}

func (s *server) createStatusMessage(j job) string {
	content := fmt.Sprintf("job `%s` status: queued", j.ID)
	msg, err := s.dg.ChannelMessageSend(j.ChannelID, content)
	if err != nil {
		log.Printf("failed to create status message for %s: %v", j.ID, err)
		return ""
	}
	return msg.ID
}

func (s *server) updateStatusMessage(j job, phase string) {
	if j.StatusMsgID == "" {
		return
	}
	content := fmt.Sprintf("job `%s` status: %s", j.ID, phase)
	_, err := s.dg.ChannelMessageEditComplex(&discordgo.MessageEdit{
		ID:      j.StatusMsgID,
		Channel: j.ChannelID,
		Content: &content,
	})
	if err != nil {
		log.Printf("failed to update status message for %s: %v", j.ID, err)
	}
}

func (s *server) workerLoop() {
	for j := range s.queue {
		s.updateStatusMessage(j, "running")
		s.markRunning(j.ID)
		var (
			result jobResult
			err    error
		)

		switch j.Type {
		case jobTypeRun:
			result, err = s.executeRunJob(j)
		case jobTypeImprove:
			result, err = s.executeRunJob(j)
		case jobTypeMerge:
			result, err = s.executeMergeJob(j)
		case jobTypeDiscard:
			result, err = s.executeDiscardJob(j)
		default:
			err = fmt.Errorf("unknown job type: %s", j.Type)
		}

		if err != nil {
			s.markDone(j.ID, statusFailed, result, err.Error())
			s.updateStatusMessage(j, "failed")
			s.notifyFailure(j, result, err)
			continue
		}
		if result.Status == "need_input" {
			s.markDone(j.ID, statusWaiting, result, "")
			s.updateStatusMessage(j, "waiting_input")
			s.notifyNeedInput(j, result)
			continue
		}

		s.markDone(j.ID, statusSucceeded, result, "")
		s.updateStatusMessage(j, "done")
		s.notifySuccess(j, result)
	}
}

func (s *server) executeRunJob(j job) (jobResult, error) {
	cmd := exec.CommandContext(context.Background(), s.cfg.RunScript, j.Repo, j.Task, j.ID)
	cmd.Dir = s.cfg.WorkDir
	cmd.Env = append(os.Environ(),
		"CHATOPS_JOB_ID="+j.ID,
		"CHATOPS_REQUESTED_BY="+j.RequestedBy,
	)
	if j.Type == jobTypeImprove && j.Branch != "" {
		cmd.Env = append(cmd.Env, "CHATOPS_TARGET_BRANCH="+j.Branch)
	}
	var out bytes.Buffer
	handleProgress := func(line string) {
		if !strings.HasPrefix(line, "PROGRESS:") {
			return
		}
		phase := strings.TrimPrefix(line, "PROGRESS:")
		s.updateStatusMessage(j, phase)
	}
	capture := &lineCapture{
		onLine: func(line string) {
			handleProgress(line)
		},
	}
	cmd.Stdout = capture
	cmd.Stderr = capture

	runErr := cmd.Run()
	capture.Flush()
	out.Write(capture.Bytes())

	outStr := out.String()
	if runErr != nil {
		res, _ := parseJobResult(out.Bytes())
		if res.LogFile == "" {
			res.LogFile = extractLogFile(outStr)
		}
		return res, fmt.Errorf("run job command failed: %w\n%s", runErr, outStr)
	}

	res, err := parseJobResult(out.Bytes())
	if err != nil {
		return jobResult{LogFile: extractLogFile(outStr)}, fmt.Errorf("failed to parse run result: %w\n%s", err, outStr)
	}
	if res.Status != "ok" && res.Status != "no_changes" && res.Status != "need_input" {
		if res.Message == "" {
			res.Message = "run job reported non-ok status"
		}
		return res, errors.New(res.Message)
	}
	return res, nil
}

func (s *server) executeMergeJob(j job) (jobResult, error) {
	cmd := exec.CommandContext(context.Background(), s.cfg.MergeScript, j.Branch, j.ID)
	cmd.Dir = s.cfg.WorkDir

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		res, _ := parseJobResult(out.Bytes())
		if res.LogFile == "" {
			res.LogFile = extractLogFile(out.String())
		}
		return res, fmt.Errorf("merge job command failed: %w\n%s", err, out.String())
	}

	res, err := parseJobResult(out.Bytes())
	if err != nil {
		return jobResult{LogFile: extractLogFile(out.String())}, fmt.Errorf("failed to parse merge result: %w\n%s", err, out.String())
	}
	if res.Status != "ok" {
		if res.Message == "" {
			res.Message = "merge job reported non-ok status"
		}
		return res, errors.New(res.Message)
	}
	return res, nil
}

func (s *server) executeDiscardJob(j job) (jobResult, error) {
	cmd := exec.CommandContext(context.Background(), s.cfg.DiscardScript, j.Branch, j.ID)
	cmd.Dir = s.cfg.WorkDir

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		res, _ := parseJobResult(out.Bytes())
		if res.LogFile == "" {
			res.LogFile = extractLogFile(out.String())
		}
		return res, fmt.Errorf("discard job command failed: %w\n%s", err, out.String())
	}

	res, err := parseJobResult(out.Bytes())
	if err != nil {
		return jobResult{LogFile: extractLogFile(out.String())}, fmt.Errorf("failed to parse discard result: %w\n%s", err, out.String())
	}
	if res.Status != "ok" {
		if res.Message == "" {
			res.Message = "discard job reported non-ok status"
		}
		return res, errors.New(res.Message)
	}
	return res, nil
}

func extractLogFile(text string) string {
	const key = "\"log_file\":"
	idx := strings.Index(text, key)
	if idx < 0 {
		return ""
	}
	rest := text[idx+len(key):]
	start := strings.Index(rest, "\"")
	if start < 0 {
		return ""
	}
	rest = rest[start+1:]
	end := strings.Index(rest, "\"")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func parseJobResult(raw []byte) (jobResult, error) {
	lines := bytes.Split(raw, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(string(lines[i]))
		if line == "" {
			continue
		}
		var r jobResult
		if err := json.Unmarshal([]byte(line), &r); err == nil {
			if r.Status == "" {
				continue
			}
			return r, nil
		}
	}
	return jobResult{}, errors.New("no JSON result found")
}

func (s *server) notifySuccess(j job, res jobResult) {
	var content string
	var components []discordgo.MessageComponent
	switch j.Type {
	case jobTypeRun:
		preview := res.PreviewURL
		if preview == "" {
			preview = "(none)"
		}
		summary := res.Summary
		if summary == "" {
			summary = "(no summary)"
		}
		content = fmt.Sprintf("<@%s>\nTask completed\n\nbranch: `%s`\npreview:\n%s\n\nsummary:\n%s", j.RequestedBy, res.Branch, preview, summary)
		s.setThreadContext(j.ChannelID, threadContext{Repo: j.Repo, Branch: res.Branch})
		components = s.quickActionComponents(j, res)
	case jobTypeImprove:
		preview := res.PreviewURL
		if preview == "" {
			preview = "(none)"
		}
		summary := res.Summary
		if summary == "" {
			summary = "(no summary)"
		}
		content = fmt.Sprintf("<@%s>\nImprove completed\n\nbranch: `%s`\npreview:\n%s\n\nsummary:\n%s", j.RequestedBy, res.Branch, preview, summary)
		s.setThreadContext(j.ChannelID, threadContext{Repo: j.Repo, Branch: res.Branch})
		components = s.quickActionComponents(j, res)
	case jobTypeMerge:
		msg := res.Message
		if msg == "" {
			msg = "merge completed"
		}
		content = fmt.Sprintf("<@%s>\nMerge completed\n\nbranch: `%s`\n%s", j.RequestedBy, j.Branch, msg)
	case jobTypeDiscard:
		msg := res.Message
		if msg == "" {
			msg = "discard completed"
		}
		content = fmt.Sprintf("<@%s>\nDiscard completed\n\nbranch: `%s`\n%s", j.RequestedBy, j.Branch, msg)
	}

	_, err := s.dg.ChannelMessageSendComplex(j.ChannelID, &discordgo.MessageSend{
		Content:    content,
		Components: components,
	})
	if err != nil {
		log.Printf("failed to send success message for %s: %v", j.ID, err)
	}
}

func (s *server) startJobThread(channelID, requestedBy, threadName, kickoffText string) string {
	msg, err := s.dg.ChannelMessageSend(channelID, fmt.Sprintf("<@%s> %s", requestedBy, kickoffText))
	if err != nil {
		log.Printf("failed to post kickoff message: %v", err)
		return channelID
	}
	thread, err := s.dg.MessageThreadStartComplex(channelID, msg.ID, &discordgo.ThreadStart{
		Name:                threadName,
		AutoArchiveDuration: 1440,
	})
	if err != nil {
		log.Printf("failed to start thread: %v", err)
		return channelID
	}
	return thread.ID
}

func (s *server) notifyFailure(j job, res jobResult, err error) {
	logPath := res.LogFile
	if logPath == "" {
		logPath = "(unknown)"
	}
	content := fmt.Sprintf("<@%s>\nJob failed: `%s`\nerror: %s\nlog: `%s`", j.RequestedBy, j.ID, err.Error(), logPath)
	var components []discordgo.MessageComponent
	if j.Type == jobTypeRun || j.Type == jobTypeImprove {
		tokenRetry := s.putUIState(uiState{
			Action:      "quick_retry",
			Repo:        j.Repo,
			Branch:      j.Branch,
			RequestedBy: j.RequestedBy,
			ChannelID:   j.ChannelID,
			Task:        j.Task,
		})
		tokenLogs := s.putUIState(uiState{
			Action:      "quick_logs",
			JobID:       j.ID,
			RequestedBy: j.RequestedBy,
			ChannelID:   j.ChannelID,
		})
		tokenDiscard := s.putUIState(uiState{
			Action:      "quick_discard",
			Branch:      j.Branch,
			RequestedBy: j.RequestedBy,
			ChannelID:   j.ChannelID,
		})
		components = []discordgo.MessageComponent{
			discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.Button{CustomID: "ui:quick:" + tokenRetry, Label: "Retry", Style: discordgo.PrimaryButton},
				discordgo.Button{CustomID: "ui:quick:" + tokenLogs, Label: "Logs", Style: discordgo.SecondaryButton},
				discordgo.Button{CustomID: "ui:quick:" + tokenDiscard, Label: "Discard", Style: discordgo.DangerButton},
			}},
		}
	}
	_, sendErr := s.dg.ChannelMessageSendComplex(j.ChannelID, &discordgo.MessageSend{
		Content:    content,
		Components: components,
	})
	if sendErr != nil {
		log.Printf("failed to send failure message for %s: %v", j.ID, sendErr)
	}
}

func (s *server) notifyNeedInput(j job, res jobResult) {
	question := strings.TrimSpace(res.Message)
	if question == "" {
		question = "Codex requires additional input."
	}
	token := s.putUIState(uiState{
		Action:      "answer_question",
		Repo:        j.Repo,
		Branch:      res.Branch,
		RequestedBy: j.RequestedBy,
		ChannelID:   j.ChannelID,
		Task:        j.Task,
		Question:    question,
	})
	content := fmt.Sprintf("<@%s>\nCodex needs your input before deploy:\n%s", j.RequestedBy, question)
	_, err := s.dg.ChannelMessageSendComplex(j.ChannelID, &discordgo.MessageSend{
		Content: content,
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.Button{CustomID: "ui:question:" + token, Label: "Answer", Style: discordgo.PrimaryButton},
			}},
		},
	})
	if err != nil {
		log.Printf("failed to send need-input message for %s: %v", j.ID, err)
	}
}

func (s *server) respond(i *discordgo.InteractionCreate, content string, ephemeral bool) {
	flags := discordgo.MessageFlags(0)
	if ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}
	err := s.dg.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   flags,
		},
	})
	if err != nil {
		log.Printf("interaction response failed: %v", err)
	}
}

func (s *server) respondWithComponents(i *discordgo.InteractionCreate, content string, ephemeral bool, comps []discordgo.MessageComponent) {
	flags := discordgo.MessageFlags(0)
	if ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}
	err := s.dg.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content:    content,
			Flags:      flags,
			Components: comps,
		},
	})
	if err != nil {
		log.Printf("interaction response with components failed: %v", err)
	}
}

func (s *server) respondModal(i *discordgo.InteractionCreate, customID, title, fieldID, placeholder string) {
	err := s.dg.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: customID,
			Title:    title,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.TextInput{
						CustomID:    fieldID,
						Label:       "Task",
						Style:       discordgo.TextInputParagraph,
						Required:    true,
						Placeholder: placeholder,
						MaxLength:   1000,
					},
				}},
			},
		},
	})
	if err != nil {
		log.Printf("modal response failed: %v", err)
	}
}

func (s *server) respondAnswerModal(i *discordgo.InteractionCreate, customID, title string) {
	err := s.dg.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: customID,
			Title:    title,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.TextInput{
						CustomID:    "answer",
						Label:       "Your answer",
						Style:       discordgo.TextInputParagraph,
						Required:    true,
						Placeholder: "Type your answer for Codex",
						MaxLength:   1000,
					},
				}},
			},
		},
	})
	if err != nil {
		log.Printf("answer modal response failed: %v", err)
	}
}

func (s *server) trackQueued(j job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[j.ID] = &jobRecord{Job: j, Status: statusQueued, UpdatedAt: time.Now()}
}

func (s *server) markRunning(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.records[jobID]; ok {
		rec.Status = statusRunning
		rec.UpdatedAt = time.Now()
	}
	s.running = jobID
}

func (s *server) markDone(jobID string, st jobStatus, res jobResult, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.records[jobID]; ok {
		rec.Status = st
		rec.Result = res
		rec.Err = errMsg
		rec.UpdatedAt = time.Now()
	}
	if s.running == jobID {
		s.running = ""
	}
}

func (s *server) sortedRecordsLocked(limit int) []jobRecord {
	out := make([]jobRecord, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func renderRecord(rec jobRecord) string {
	lines := []string{
		fmt.Sprintf("id: %s", rec.Job.ID),
		fmt.Sprintf("type: %s", rec.Job.Type),
		fmt.Sprintf("status: %s", rec.Status),
		fmt.Sprintf("updated: %s", rec.UpdatedAt.Format(time.RFC3339)),
	}
	if rec.Result.Branch != "" {
		lines = append(lines, fmt.Sprintf("branch: %s", rec.Result.Branch))
	}
	if rec.Result.PreviewURL != "" {
		lines = append(lines, fmt.Sprintf("preview: %s", rec.Result.PreviewURL))
	}
	if rec.Result.LogFile != "" {
		lines = append(lines, fmt.Sprintf("log: %s", rec.Result.LogFile))
	}
	if rec.Err != "" {
		lines = append(lines, fmt.Sprintf("error: %s", rec.Err))
	}
	return strings.Join(lines, "\n")
}

func (s *server) putUIState(st uiState) string {
	token := fmt.Sprintf("%x", time.Now().UnixNano())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uiState[token] = st
	return token
}

func (s *server) takeUIState(token string) (uiState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.uiState[token]
	if ok {
		delete(s.uiState, token)
	}
	return st, ok
}

func (s *server) setThreadContext(threadID string, ctx threadContext) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threads[threadID] = ctx
}

func (s *server) getThreadContext(threadID string) (threadContext, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, ok := s.threads[threadID]
	return ctx, ok
}

func (s *server) allowedRepoList() []string {
	repos := make([]string, 0, len(s.cfg.AllowedRepos))
	for r := range s.cfg.AllowedRepos {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	return repos
}

func (s *server) defaultRepo() string {
	repos := s.allowedRepoList()
	if len(repos) == 0 {
		return ""
	}
	return repos[0]
}

func (s *server) listTaskBranches() ([]string, error) {
	cmd := exec.Command("git", "ls-remote", "--heads", "origin", "task/*")
	cmd.Dir = s.cfg.WorkDir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	branches := make([]string, 0, len(lines))
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		fields := strings.Fields(ln)
		if len(fields) < 2 {
			continue
		}
		ref := fields[1]
		const pfx = "refs/heads/"
		if strings.HasPrefix(ref, pfx) {
			branches = append(branches, strings.TrimPrefix(ref, pfx))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(branches)))
	if len(branches) > 25 {
		branches = branches[:25]
	}
	return branches, nil
}

func (s *server) previewURL(branch string) string {
	if s.cfg.PreviewURLTpl == "" {
		return "(CHATOPS_PREVIEW_URL_TEMPLATE not configured)"
	}
	url := strings.ReplaceAll(s.cfg.PreviewURLTpl, "{branch}", branch)
	url = strings.ReplaceAll(url, "{branch_slug}", sanitizeBranch(branch))
	return url
}

func (s *server) quickActionComponents(j job, res jobResult) []discordgo.MessageComponent {
	tokenImprove := s.putUIState(uiState{
		Action:      "quick_improve",
		Repo:        j.Repo,
		Branch:      res.Branch,
		RequestedBy: j.RequestedBy,
		ChannelID:   j.ChannelID,
	})
	tokenMerge := s.putUIState(uiState{
		Action:      "quick_merge",
		Branch:      res.Branch,
		RequestedBy: j.RequestedBy,
		ChannelID:   j.ChannelID,
	})
	tokenDiscard := s.putUIState(uiState{
		Action:      "quick_discard",
		Branch:      res.Branch,
		RequestedBy: j.RequestedBy,
		ChannelID:   j.ChannelID,
	})
	tokenLogs := s.putUIState(uiState{
		Action:      "quick_logs",
		JobID:       j.ID,
		RequestedBy: j.RequestedBy,
		ChannelID:   j.ChannelID,
	})

	row := []discordgo.MessageComponent{
		discordgo.Button{CustomID: "ui:quick:" + tokenImprove, Label: "Improve", Style: discordgo.PrimaryButton},
		discordgo.Button{CustomID: "ui:quick:" + tokenMerge, Label: "Merge", Style: discordgo.SuccessButton},
		discordgo.Button{CustomID: "ui:quick:" + tokenDiscard, Label: "Discard", Style: discordgo.DangerButton},
		discordgo.Button{CustomID: "ui:quick:" + tokenLogs, Label: "Logs", Style: discordgo.SecondaryButton},
	}
	comps := []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: row},
	}
	if res.PreviewURL != "" {
		comps = append(comps, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "Open Preview", Style: discordgo.LinkButton, URL: res.PreviewURL},
		}})
	}
	return comps
}

func modalInputValue(rows []discordgo.MessageComponent, customID string) string {
	for _, row := range rows {
		r, ok := row.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, comp := range r.Components {
			ti, ok := comp.(*discordgo.TextInput)
			if !ok {
				continue
			}
			if ti.CustomID == customID {
				return strings.TrimSpace(ti.Value)
			}
		}
	}
	return ""
}

func containsMention(m *discordgo.MessageCreate, userID string) bool {
	for _, u := range m.Mentions {
		if u != nil && u.ID == userID {
			return true
		}
	}
	return false
}

func stripMentionsAndTrim(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	parts := strings.Fields(s)
	filtered := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.HasPrefix(p, "<@") && strings.HasSuffix(p, ">") {
			continue
		}
		filtered = append(filtered, p)
	}
	return strings.TrimSpace(strings.Join(filtered, " "))
}

func combineTaskAndAnswer(task, question, answer string) string {
	task = strings.TrimSpace(task)
	question = strings.TrimSpace(question)
	answer = strings.TrimSpace(answer)
	if question == "" {
		return task + "\n\nUser answer:\n" + answer
	}
	return task + "\n\nQuestion from Codex:\n" + question + "\n\nUser answer:\n" + answer
}

func optionString(options []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, opt := range options {
		if opt.Name == name {
			return opt.StringValue()
		}
	}
	return ""
}

func optionInt(options []*discordgo.ApplicationCommandInteractionDataOption, name string, fallback int) int {
	for _, opt := range options {
		if opt.Name == name {
			return int(opt.IntValue())
		}
	}
	return fallback
}

func requesterID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

func fallback(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func sanitizeBranch(branch string) string {
	branch = strings.ToLower(branch)
	repl := strings.NewReplacer("/", "-", "_", "-", " ", "-", ".", "-")
	branch = repl.Replace(branch)
	var b strings.Builder
	for _, r := range branch {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func tailFile(path string, lines int) (string, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	buf := make([]string, 0, lines)
	for scanner.Scan() {
		buf = append(buf, scanner.Text())
		if len(buf) > lines {
			buf = buf[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return strings.Join(buf, "\n"), nil
}
