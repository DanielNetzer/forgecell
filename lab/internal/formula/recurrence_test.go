package formula

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestRecurrenceThresholdDefaultsToThree(t *testing.T) {
	f, err := Parse([]byte(recipe))
	if err != nil {
		t.Fatal(err)
	}
	if f.Learning != nil || f.RecurrenceThreshold() != 3 || DefaultRecurrenceThreshold != 3 {
		t.Fatalf("%+v %d", f.Learning, f.RecurrenceThreshold())
	}
	if f.YAML != recipe || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(recipe))) {
		t.Fatal("a Formula without the setting must keep its bytes and SHA-256")
	}
	empty, err := Parse([]byte(recipe + "learning: {}\n"))
	if err != nil || empty.RecurrenceThreshold() != 3 {
		t.Fatalf("%v %d", err, empty.RecurrenceThreshold())
	}
}

func TestRecurrenceThresholdIsConfigurable(t *testing.T) {
	for _, n := range []int{1, 2, 5, MaxRecurrenceThreshold} {
		text := fmt.Sprintf("%slearning:\n  recurrenceThreshold: %d\n", recipe, n)
		f, err := Parse([]byte(text))
		if err != nil {
			t.Fatalf("%d: %v", n, err)
		}
		if f.RecurrenceThreshold() != n || f.YAML != text {
			t.Fatalf("%d: got %d", n, f.RecurrenceThreshold())
		}
	}
}

func TestRecurrenceThresholdRejectsInvalidValues(t *testing.T) {
	for name, value := range map[string]string{
		"zero":     "0",
		"negative": "-3",
		"too big":  fmt.Sprint(MaxRecurrenceThreshold + 1),
		"huge":     "18446744073709551615",
		"float":    "2.5",
		"string":   `"3"`,
		"null":     "null",
		"boolean":  "true",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(recipe + "learning:\n  recurrenceThreshold: " + value + "\n"))
			if err == nil || !strings.Contains(err.Error(), "recurrenceThreshold") {
				t.Fatalf("want a recurrenceThreshold error, got %v", err)
			}
		})
	}
	for name, text := range map[string]string{
		"scalar":      recipe + "learning: 3\n",
		"list":        recipe + "learning: [3]\n",
		"unknown key": recipe + "learning:\n  threshold: 3\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(text)); err == nil || !strings.Contains(err.Error(), "learning") {
				t.Fatalf("want a learning error, got %v", err)
			}
		})
	}
}
