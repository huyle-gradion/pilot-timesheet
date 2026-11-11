package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

type pendingAnswer struct {
	Kind      string
	Digest    string
	Answer    string
	Surface   string
	StoreArgs []string
}

type answerReceipt struct {
	Day        string `json:"day"`
	Kind       string `json:"kind"`
	Answer     string `json:"answer"`
	Surface    string `json:"surface"`
	AnswerFile string `json:"answer_file"`
	Next       string `json:"next"`
}

func answerable(q *askHuman) error {
	if q.Kind == askResolve {
		return fmt.Errorf("the resolve ask needs the reconcile receipt's identifiers, so it has no one-word answer; " +
			"write its decision file as the protocol shows, or settle the write with `timesheet submit resolve`")
	}
	if len(q.Options) == 0 {
		return fmt.Errorf("the %s ask has no closed options to answer from", q.Kind)
	}
	return nil
}

// answerAsk re-derives the day over the caller's open store and writes the
// answer to the current ask's file only when it would be accepted: the kind
// matches, the digest is the current one (an empty Digest copies it) and the
// answer is one of the closed options. A refusal writes nothing.
func answerAsk(cfg *timesheet.Config, configPath, envFile, day string, store *timesheet.Store, p pendingAnswer, confirm func(*askHuman) error) (answerReceipt, error) {
	obs, stop, err := buildNextObservation(cfg, configPath, envFile, day, store)
	if err != nil {
		return answerReceipt{}, err
	}
	a := nextAction{Day: day, State: stateStopped}
	if stop != nil {
		a = *stop
	} else {
		a = deriveDayNext(obs)
	}
	q := a.Ask
	if a.State != stateAskHuman || q == nil {
		return answerReceipt{}, fmt.Errorf("no ask_human question is pending for %s (the day is at %s); nothing was written", day, a.State)
	}
	if p.Kind != "" && p.Kind != q.Kind {
		return answerReceipt{}, fmt.Errorf("the pending ask is %s, not %s; nothing was written", q.Kind, p.Kind)
	}
	if err := answerable(q); err != nil {
		return answerReceipt{}, err
	}
	digest := p.Digest
	if digest == "" {
		digest = q.Digest
	}
	if problem := askProblem(askAnswer{Present: true, Digest: digest, Answer: p.Answer, Surface: p.Surface}, q.Digest, q.Options); problem != "" {
		return answerReceipt{}, fmt.Errorf("the answer was refused and nothing was written; last_error: %s", problem)
	}
	if confirm != nil {
		if err := confirm(q); err != nil {
			return answerReceipt{}, err
		}
	}
	body, err := json.Marshal(map[string]string{"digest": digest, "answer": p.Answer, "surface": p.Surface})
	if err != nil {
		return answerReceipt{}, err
	}
	if err := os.MkdirAll(filepath.Dir(q.AnswerFile), 0o700); err != nil {
		return answerReceipt{}, fmt.Errorf("the answer folder cannot be created")
	}
	if err := writeFileAtomic(q.AnswerFile, append(body, '\n')); err != nil {
		return answerReceipt{}, fmt.Errorf("the answer file cannot be written")
	}
	return answerReceipt{
		Day: day, Kind: q.Kind, Answer: p.Answer, Surface: p.Surface, AnswerFile: q.AnswerFile,
		Next: dayCommand([]string{"log"}, p.StoreArgs, day),
	}, nil
}

func storeFlagArgs(configPath, envFile string) []string {
	var out []string
	if configPath != "" {
		out = append(out, "--config", configPath)
	}
	if envFile != "" {
		out = append(out, "--env-file", envFile)
	}
	return out
}

func dayCommand(verb, store []string, day string) string {
	argv := append(append([]string{"timesheet"}, verb...), store...)
	return shellJoin(append(argv, "--date", day))
}

func confirmAnswerOnTerminal(term *interactive, day string, q *askHuman, answer string) error {
	prompt := recorded("About to answer the %s question for %s:\n  %s\noptions: %s\nanswer: %s",
		q.Kind, day, q.Question, strings.Join(q.Options, ", "), answer)
	return term.confirm(prompt, answer)
}

func runAnswer(args []string) error {
	flags := flag.NewFlagSet("answer", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var configPath, envFile, date, kind string
	flags.StringVar(&configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.StringVar(&date, "date", "", "day whose ask_human question to answer, YYYY-MM-DD (required)")
	flags.StringVar(&kind, "kind", "", "the ask kind being answered (optional; refused when it is not the pending one)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || strings.TrimSpace(flags.Arg(0)) == "" {
		return fmt.Errorf("answer takes exactly one answer after its flags: timesheet answer --date <YYYY-MM-DD> [--kind <kind>] <answer>")
	}
	if date == "" {
		return fmt.Errorf("answer requires --date")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return fmt.Errorf("answer: --date must be YYYY-MM-DD")
	}
	explicit, err := absStoreFlags(configPath, envFile)
	if err != nil {
		return fmt.Errorf("answer: %w", err)
	}
	configPath, envFile, err = resolveStoreFlags("answer", configPath, envFile)
	if err != nil {
		return err
	}
	term, err := interactiveFactory("answer", "answer an ask_human question")
	if err != nil {
		return err
	}
	if configPath, err = filepath.Abs(configPath); err != nil {
		return fmt.Errorf("answer: the configuration path cannot be made absolute")
	}
	if envFile != "" {
		if envFile, err = filepath.Abs(envFile); err != nil {
			return fmt.Errorf("answer: the environment-file path cannot be made absolute")
		}
	}
	cfg, err := timesheet.LoadConfig(configPath)
	if err != nil {
		return err
	}
	store, err := timesheet.OpenStore(cfg.StatePath)
	if err != nil {
		return err
	}
	answer := flags.Arg(0)
	receipt, err := answerAsk(cfg, configPath, envFile, date, store, pendingAnswer{
		Kind: kind, Answer: answer, Surface: "terminal", StoreArgs: explicit,
	}, func(q *askHuman) error { return confirmAnswerOnTerminal(term, date, q, answer) })
	if closeErr := store.Close(); closeErr != nil && err == nil {
		err = fmt.Errorf("answer: the store lock cannot be released")
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(term.out, "answered %s; run `%s` to continue\n", receipt.Kind, receipt.Next)
	return printJSON(receipt)
}

// absStoreFlags is the explicitly given --config/--env-file as absolute
// paths, for the commands a human copies; defaults are left out.
func absStoreFlags(configPath, envFile string) ([]string, error) {
	var err error
	if configPath != "" {
		if configPath, err = filepath.Abs(configPath); err != nil {
			return nil, fmt.Errorf("the configuration path cannot be made absolute")
		}
	}
	if envFile != "" {
		if envFile, err = filepath.Abs(envFile); err != nil {
			return nil, fmt.Errorf("the environment-file path cannot be made absolute")
		}
	}
	return storeFlagArgs(configPath, envFile), nil
}
