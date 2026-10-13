package formula

import "fmt"

// DefaultRecurrenceThreshold is how many consecutive same-class failures fire a
// learning recurrence rule when the Formula does not set one.
const DefaultRecurrenceThreshold = 3

// MaxRecurrenceThreshold bounds the setting so a rule can still fire within the
// recent-ledger window the Lab reads.
const MaxRecurrenceThreshold = 50

// LearningSettings holds optional learning settings. Absent keeps the Formula
// bytes, SHA and behaviour of earlier Formulas unchanged.
type LearningSettings struct {
	RecurrenceThreshold int `yaml:"recurrenceThreshold" json:"recurrenceThreshold,omitempty"`
}

// RecurrenceThreshold is the reviewed threshold, or the default when unset.
func (f Formula) RecurrenceThreshold() int {
	if f.Learning == nil || f.Learning.RecurrenceThreshold == 0 {
		return DefaultRecurrenceThreshold
	}
	return f.Learning.RecurrenceThreshold
}

func validateLearning(value any) error {
	m, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("learning must be a mapping; use recurrenceThreshold")
	}
	for key, v := range m {
		if key != "recurrenceThreshold" {
			return fmt.Errorf("learning: unknown key %q; use recurrenceThreshold", key)
		}
		var n int64
		switch x := v.(type) {
		case int:
			n = int64(x)
		case int64:
			n = x
		case uint64:
			if x > MaxRecurrenceThreshold {
				n = MaxRecurrenceThreshold + 1
			} else {
				n = int64(x)
			}
		default:
			return fmt.Errorf("learning.recurrenceThreshold must be an integer from 1 to %d", MaxRecurrenceThreshold)
		}
		if n < 1 || n > MaxRecurrenceThreshold {
			return fmt.Errorf("learning.recurrenceThreshold must be an integer from 1 to %d", MaxRecurrenceThreshold)
		}
	}
	return nil
}
