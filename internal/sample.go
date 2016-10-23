package config

import (
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"

	strftime "github.com/cactus/gostrftime"
	log "github.com/coccyx/gogen/logger"
	"github.com/satori/go.uuid"
	lua "github.com/yuin/gopher-lua"
)

const randStringLetters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
const randHexLetters = "ABCDEF0123456789"

// Sample is the main configuration data structure which is passed around through Gogen
// Publicly exported options are brought in through YAML or JSON configs, and some state is maintained in private unexposed variables.
type Sample struct {
	Name            string              `json:"name"`
	Description     string              `json:"description,omitempty"`
	Notes           string              `json:"notes,omitempty"`
	Disabled        bool                `json:"disabled"`
	Generator       string              `json:"generator,omitempty"`
	RaterString     string              `json:"rater,omitempty"`
	Interval        int                 `json:"interval,omitempty"`
	Delay           int                 `json:"delay,omitempty"`
	Count           int                 `json:"count,omitempty"`
	Earliest        string              `json:"earliest,omitempty"`
	Latest          string              `json:"latest,omitempty"`
	Begin           string              `json:"begin,omitempty"`
	End             string              `json:"end,omitempty"`
	EndIntervals    int                 `json:"endIntervals,omitempty"`
	RandomizeCount  float32             `json:"randomizeCount,omitempty"`
	RandomizeEvents bool                `json:"randomizeEvents,omitempty"`
	Tokens          []Token             `json:"tokens,omitempty"`
	Lines           []map[string]string `json:"lines,omitempty"`
	Field           string              `json:"field,omitempty"`
	FromSample      string              `json:"fromSample,omitempty"`
	SinglePass      bool                `json:"singlepass,omitempty"`

	// Internal use variables
	Gen            Generator                    `json:"-"`
	Out            Outputter                    `json:"-"`
	Rater          Rater                        `json:"-"`
	Output         *Output                      `json:"-"`
	EarliestParsed time.Duration                `json:"-"`
	LatestParsed   time.Duration                `json:"-"`
	BeginParsed    time.Time                    `json:"-"`
	EndParsed      time.Time                    `json:"-"`
	Current        time.Time                    `json:"-"` // If we are backfilling or generating for a specified time window, what time is it?
	Realtime       bool                         `json:"-"` // Are we done doing batch backfill or specified time window?
	BrokenLines    []map[string][]StringOrToken `json:"-"`
	realSample     bool                         // Used to represent samples which aren't just used to store lines from CSV or raw
}

// Clock allows for implementers to keep track of their own view
// of current time.  In Gogen, this is used for being able to generate
// events between certain time windows, or backfill from a certain time
// while continuing to run in real time.
type Clock interface {
	Now() time.Time
}

// Now returns the current time for the sample, and handles
// whether we are currently generating a backfill or
// specified time window or whether we should be generating
// events in realtime
func (s *Sample) Now() time.Time {
	if !s.Realtime {
		return s.Current
	}
	return time.Now()
}

// Token describes a replacement task to run against a sample
type Token struct {
	Name           string              `json:"name"`
	Format         string              `json:"format"`
	Token          string              `json:"token"`
	Type           string              `json:"type"`
	Replacement    string              `json:"replacement,omitempty"`
	Group          int                 `json:"group,omitempty"`
	Sample         *Sample             `json:"-"`
	Parent         *Sample             `json:"-"`
	SampleString   string              `json:"sample,omitempty"`
	Field          string              `json:"field,omitempty"`
	SrcField       string              `json:"srcField,omitempty"`
	Precision      int                 `json:"precision,omitempty"`
	Lower          int                 `json:"lower,omitempty"`
	Upper          int                 `json:"upper,omitempty"`
	Length         int                 `json:"length,omitempty"`
	WeightedChoice []WeightedChoice    `json:"weightedChoice,omitempty"`
	FieldChoice    []map[string]string `json:"fieldChoice,omitempty"`
	Choice         []string            `json:"choice,omitempty"`
	Script         string              `json:"script,omitempty"`

	L        *lua.LState `json:"-"`
	luaState *lua.LTable
}

// WeightedChoice is a simple data structure for allowing a list of items with a Choice to pick and a Weight for that choice
type WeightedChoice struct {
	Weight int    `json:"weight"`
	Choice string `json:"choice"`
}

type tokenpos struct {
	Pos1  int
	Pos2  int
	Token int
}

type tokenspos []tokenpos

func (tp tokenspos) Len() int           { return len(tp) }
func (tp tokenspos) Less(i, j int) bool { return tp[i].Pos1 < tp[j].Pos2 }
func (tp tokenspos) Swap(i, j int)      { tp[i], tp[j] = tp[j], tp[i] }

type StringOrToken struct {
	S string
	T *Token
}

// Replace replaces any instances of this token in the string pointed to by event.  Since time is native is Gogen, we can pass in
// earliest and latest time ranges to generate the event between.  Lastly, some times we want to span a selected choice over multiple
// tokens.  Passing in a pointer to choice allows the replacement to choose a preselected row in FieldChoice or Choice.
func (t Token) Replace(event *string, choice *int64, et time.Time, lt time.Time, randgen *rand.Rand) error {
	// s := t.Sample
	e := *event

	if pos1, pos2, err := t.GetReplacementOffsets(*event); err != nil {
		return nil
	} else {
		replacement, err := t.GenReplacement(choice, et, lt, randgen)
		if err != nil {
			return err
		}
		*event = e[:pos1] + replacement + e[pos2:]
		return nil
	}
}

// GetReplacementOffsets returns the beginning and end of a token inside an event string
func (t Token) GetReplacementOffsets(event string) (int, int, error) {
	switch t.Format {
	case "template":
		if pos := strings.Index(event, t.Token); pos >= 0 {
			return pos, pos + len(t.Token), nil
		}
	case "regex":
		re, err := regexp.Compile(t.Token)
		if err != nil {
			return -1, -1, err
		}
		match := re.FindStringSubmatchIndex(event)
		if match != nil && len(match) >= 4 {
			return match[2], match[3], nil
		}
	}
	return -1, -1, fmt.Errorf("Token '%s' not found in field '%s': '%s'", t.Token, t.Field, event)
}

// GenReplacement generates a replacement value for the token.  choice allows the user to specify
// a specific value to choose in the array.  This is useful for saving picks amongst tokens.
func (t Token) GenReplacement(choice *int64, et time.Time, lt time.Time, randgen *rand.Rand) (string, error) {
	c := *choice
	switch t.Type {
	case "timestamp", "gotimestamp", "epochtimestamp":
		var replacementTime time.Time
		if c == -1 {
			td := lt.Sub(et)

			var tdr int
			if int(td) > 0 {
				tdr = randgen.Intn(int(td))
			}
			rd := time.Duration(tdr)
			replacementTime = lt.Add(rd * -1)
			*choice = replacementTime.UnixNano()
		} else {
			replacementTime = time.Unix(0, c)
		}
		switch t.Type {
		case "timestamp":
			return strftime.Format(t.Replacement, replacementTime), nil
		case "gotimestamp":
			return replacementTime.Format(t.Replacement), nil
		case "epochtimestamp":
			return strconv.FormatInt(replacementTime.Unix(), 10), nil
		}
	case "static":
		return t.Replacement, nil
	case "random":
		switch t.Replacement {
		case "int":
			offset := 0 - t.Lower
			return strconv.Itoa(randgen.Intn(t.Upper-offset) + offset), nil
		case "float":
			lower := t.Lower * int(math.Pow10(t.Precision))
			offset := 0 - lower
			upper := t.Upper * int(math.Pow10(t.Precision))
			f := float64(randgen.Intn(upper-offset)+offset) / math.Pow10(t.Precision)
			return strconv.FormatFloat(f, 'f', t.Precision, 64), nil
		case "string", "hex":
			b := make([]byte, t.Length)
			var l string
			if t.Replacement == "string" {
				l = randStringLetters
			} else {
				l = randHexLetters
			}
			for i := range b {
				b[i] = l[randgen.Intn(len(l))]
			}
			return string(b), nil
		case "guid":
			u := uuid.NewV4()
			return u.String(), nil
		case "ipv4":
			var ret string
			for i := 0; i < 4; i++ {
				ret = ret + strconv.Itoa(randgen.Intn(255)) + "."
			}
			ret = strings.TrimRight(ret, ".")
			return ret, nil
		case "ipv6":
			var ret string
			for i := 0; i < 8; i++ {
				ret = ret + fmt.Sprintf("%x", randgen.Intn(65535)) + ":"
			}
			ret = strings.TrimRight(ret, ":")
			return ret, nil
		}
	case "choice":
		if c == -1 {
			c = int64(randgen.Intn(len(t.Choice)))
			*choice = c
		}
		return t.Choice[c], nil
	case "weightedChoice":
		// From http://eli.thegreenplace.net/2010/01/22/weighted-random-generation-in-python/
		var totals []int
		runningTotal := 0

		for _, w := range t.WeightedChoice {
			runningTotal += w.Weight
			totals = append(totals, runningTotal)
		}

		r := randgen.Float64() * float64(runningTotal)
		for j, total := range totals {
			if r < float64(total) {
				*choice = int64(j)
				break
			}
		}
		return t.WeightedChoice[*choice].Choice, nil
	case "fieldChoice":
		if c == -1 {
			c = int64(randgen.Intn(len(t.FieldChoice)))
			*choice = c
		}
		return t.FieldChoice[c][t.SrcField], nil
	case "script":
		L := lua.NewState()
		defer L.Close()
		L.SetGlobal("state", t.luaState)
		if err := L.DoString(t.Script); err != nil {
			log.Errorf("Error executing script for token '%s' in sample '%s': %s", t.Name, t.Parent.Name, err)
		}
		return lua.LVAsString(L.Get(-1)), nil
	}
	return "", fmt.Errorf("GenReplacement called with invalid type for token '%s' with type '%s'", t.Name, t.Type)
}
