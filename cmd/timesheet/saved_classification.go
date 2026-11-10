package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func checkSavedClassificationBinding(h *headlessSession, date string, data []byte) error {
	var answer struct {
		LedgerDigest json.RawMessage `json:"ledger_digest"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return fmt.Errorf("classify: invalid binding: %w", err)
	}
	if len(answer.LedgerDigest) == 0 {
		return nil
	}
	var binding string
	if err := json.Unmarshal(answer.LedgerDigest, &binding); err != nil {
		return fmt.Errorf("classify: invalid ledger_digest: %w", err)
	}
	digest, err := h.session.SavedClassificationDigest(date)
	if err != nil {
		return err
	}
	if binding != digest {
		return fmt.Errorf("classify: ledger_digest does not match the saved day; request a fresh classification judgment")
	}
	return nil
}

type classificationAnswer struct {
	Present        bool
	Binding        string
	Selections     map[string]timesheet.ClassificationChoiceEntry
	SelectionError error
	SHA            string
}

func readClassificationAnswer(path string) (classificationAnswer, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return classificationAnswer{}, nil
	}
	if err != nil {
		return classificationAnswer{}, err
	}
	var answer struct {
		LedgerDigest string `json:"ledger_digest"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return classificationAnswer{Present: true, SHA: answerSHA(data)}, nil
	}
	choices, selectionErr := timesheet.ParseClassificationChoices(data)
	return classificationAnswer{
		Present: true, Binding: answer.LedgerDigest, SHA: answerSHA(data),
		Selections: choices, SelectionError: selectionErr,
	}, nil
}
