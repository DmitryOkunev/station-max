package bridge

import (
	"context"
	"fmt"
	"quasarbridge/internal/playback"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// SmartTubeController controls YouTube/SmartTube, Zona, LazyMedia playback and TV app navigation.
type SmartTubeController interface {
	CanLaunchApp(text string) (app string, ok bool)
	LaunchApp(ctx context.Context, app string) (string, error)
	CanPlay(text string) (query string, ok bool)
	Play(ctx context.Context, query string) (string, error)
	CanSearchApp(text string) (app string, query string, ok bool)
	SearchApp(ctx context.Context, app string, query string) (string, error)
	Command(text string) string
	Active(ctx context.Context) bool
	ControlMedia(ctx context.Context, action string) (string, error)
	PauseBackground()
}

var (
	// Universal media commands patterns
	mediaNextRegex = regexp.MustCompile(`^(?:(?:включи|поставь|переключи|переключись|перейди|давай)(?: на)? )?(?:дальше|вперед|следующ(?:ее|ий|ая|ую)(?: (?:видео|ролик|клип|трек|песн[юя]|сери[юя]))?|переключи(?: (?:видео|ролик|клип|трек))?|пропусти(?: (?:это|видео|ролик|клип|трек|песню))?|скип(?:ни)?)$`)

	mediaPrevRegex = regexp.MustCompile(`^(?:(?:включи|поставь|верни|переключи|переключись|перейди|давай)(?: на)? )?(?:назад|предыдущ(?:ее|ий|ая|ую)(?: (?:видео|ролик|клип|трек|песн[юя]|сери[юя]))?|верни(?: назад)?)$`)

	mediaCloseRegex = regexp.MustCompile(`^(?:закрой(?:ся)?|выйди|выруби|выключи(?:сь)?)(?: (?:ютуб|youtube|смарттьюб|smarttube|зону|zona|медиа|лейзи|lazy|видео|ролик|приложение|из (?:ютуба|youtube|зоны|медиа|приложения)|домой|в меню))?$`)

	mediaPauseRegex = regexp.MustCompile(`^(?:пауза|на паузу|(?:поставь|поставьте)(?: (?:видео|ролик|воспроизведение|музыку|песню|трек))? на паузу|приостанови(?: (?:видео|ролик|воспроизведение|музыку|песню|трек))?|стоп|хватит|останови(?:сь)?(?: (?:видео|ролик|воспроизведение|музыку|песню|трек))?)$`)

	mediaResumeRegex = regexp.MustCompile(`^(?:продолжи(?:ть)?|продолжай|возобнови(?:ть)?|играй(?: дальше)?|воспроизведи|(?:сними|снять)(?: (?:видео|ролик|воспроизведение))? с паузы)(?: (?:видео|ролик|воспроизведение|музыку|песню|трек))?$`)

	mediaRestartRegex = regexp.MustCompile(`^(?:сначала|с самого начала|начни(?: (?:видео|ролик|песню|трек))? сначала|включи(?: (?:видео|ролик|песню|трек))? сначала|повтори(?: (?:видео|ролик|песню|трек))?)$`)

	mediaStatusRegex = regexp.MustCompile(`^(?:что(?: сейчас)? играет|какое(?: сейчас)? видео играет|что за (?:видео|ролик)|что это за ролик|какой(?: сейчас)? ролик играет)$`)

	numPattern     = `(?:\d+(?:-[а-я]+)?|полтора|один|одну|одна|два|две|три|четыре|пять|шесть|семь|восемь|девять|десять|перв[а-я]*|втор[а-я]*|трет[а-я]*|четверт[а-я]*|пят[а-я]*|шест[а-я]*|седьм[а-я]*|восьм[а-я]*|девят[а-я]*|десят[а-я]*|одиннадцат[а-я]*|двенадцат[а-я]*|двадцат[а-я]*|тридцат[а-я]*)`
	ordinalPattern = `(?:\d+-(?:ую|ей|ю|й|я)|перв[а-я]*|втор[а-я]*|трет[а-я]*|четверт[а-я]*|пят[а-я]*|шест[а-я]*|седьм[а-я]*|восьм[а-я]*|девят[а-я]*|десят[а-я]*|одиннадцат[а-я]*|двенадцат[а-я]*|двадцат[а-я]*|тридцат[а-я]*)`

	seekDirShortRegex        = regexp.MustCompile(`^(вперед|назад|дальше)(?: на)?(?: (` + numPattern + `))? *(секунд[а-я]*|сек|минут[а-я]*|мин|час[а-я]*)?$`)
	seekDirCmdRegex          = regexp.MustCompile(`^(?:(?:перемотай|мотни|прокрути)(?: (?:на|ролик|видео|запись))* )?(?:на )?(` + numPattern + `)? *(секунд[а-я]*|сек|минут[а-я]*|мин|час[а-я]*)? *(вперед|назад|дальше)$`)

	seekHourRegex            = regexp.MustCompile(`^(?:(?:перемотай|перейди|поставь|включи|начни|мотни|прокрути)(?: ролик| видео)? )?(?:на|с) (` + numPattern + `) час[а-я]*(?: (` + numPattern + `) минут[а-я]*)?(?: (` + numPattern + `) секунд[а-я]*)?$`)
	seekMinSecRegex          = regexp.MustCompile(`^(?:(?:перемотай|перейди|поставь|включи|начни|мотни|прокрути)(?: ролик| видео)? )?(?:на|с) (` + numPattern + `) минут[а-я]* (` + numPattern + `) секунд[а-я]*$`)
	seekFromMinRegex         = regexp.MustCompile(`^(?:(?:перемотай|перейди|поставь|включи|начни|мотни|прокрути)(?: ролик| видео)? )?с (` + numPattern + `) минут[а-я]*$`)
	seekTimelineOrdinalRegex = regexp.MustCompile(`^(?:(?:перемотай|мотни|прокрути)(?: ролик| видео)? )?на (` + ordinalPattern + `) минут[а-я]*$`)
	seekTimelineActionRegex  = regexp.MustCompile(`^(?:поставь|перейди|начни)(?: (?:ролик|видео))? на (` + numPattern + `) минут[а-я]*$`)
	seekClockRegex           = regexp.MustCompile(`^(?:(?:перемотай|перейди|поставь|включи|начни|мотни|прокрути)(?: ролик| видео)? )?(?:на|с) (?:(\d+)[: ])?(\d+)[: ](\d+)$`)
	seekSecRegex             = regexp.MustCompile(`^(?:(?:перемотай|перейди|поставь|включи|начни|мотни|прокрути)(?: ролик| видео)? )?(?:на|с) (` + numPattern + `) секунд[уе]$`)
	seekSimpleRegex          = regexp.MustCompile(`^(?:(?:перемотай|мотни|прокрути)(?: (?:на|ролик|видео|запись))* )?(?:на )?(` + numPattern + `)? *(секунд[а-я]*|сек|минут[а-я]*|мин|час[а-я]*)?$`)

	appZonaRegex  = regexp.MustCompile(`^(?:(?:включи|открой|запусти|покажи|вруби)(?: приложение)?)? ?(?:зону|зона|zona)$`)
	appMediaRegex = regexp.MustCompile(`^(?:(?:включи|открой|запусти|покажи|вруби)(?: приложение)?)? ?(?:медиа)$`)
	appLazyRegex  = regexp.MustCompile(`^(?:(?:включи|открой|запусти|покажи|вруби)(?: приложение)?)? ?(?:лейзи|лейзи медиа|lazy|lazymedia|lazy media)$`)
	appHomeRegex  = regexp.MustCompile(`^(?:домой|на главный экран|главный экран|на домашний экран|домашний экран|(?:выйди|вернись|перейди)(?: домой| на главный экран| на домашний экран)|(?:открой|включи|покажи|запусти)(?: главный экран| домашний экран))$`)

	tvActionPrefixes = []string{
		"найди", "поищи", "открой", "включи", "поставь", "запусти", "покажи", "вруби",
	}

	smartHomeFilterPrefixes = []string{
		"свет", "ламп", "люстр", "розетк", "штор", "кондиционер", "пылесос", "выключател", "термостат",
	}
)

// cleanText normalizes speech recognition text: lowercases, replaces 'ё', removes punctuation and Alice greetings.
func cleanText(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	text = strings.ReplaceAll(text, "ё", "е")
	text = strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) && r != '-' && r != ':' {
			return ' '
		}
		return r
	}, text)
	words := strings.Fields(text)
	for len(words) > 0 && (words[0] == "алиса" || words[0] == "alice" || words[0] == "пожалуйста") {
		words = words[1:]
	}
	for len(words) > 0 && words[len(words)-1] == "пожалуйста" {
		words = words[:len(words)-1]
	}
	return strings.Join(words, " ")
}

// SmartTubeAppCommand recognizes app launch commands (Zona, LazyMedia) and Home navigation.
func SmartTubeAppCommand(text string) (string, bool) {
	norm := cleanText(text)
	if norm == "" {
		return "", false
	}

	if appHomeRegex.MatchString(norm) {
		return "home", true
	}
	if appZonaRegex.MatchString(norm) {
		return "zona", true
	}
	if appMediaRegex.MatchString(norm) {
		return "media", true
	}
	if appLazyRegex.MatchString(norm) {
		return "lazy", true
	}

	return "", false
}

var ordinalsMap = map[string]int{
	"перв": 1, "втор": 2, "трет": 3, "четверт": 4, "пят": 5, "шест": 6,
	"седьм": 7, "восьм": 8, "девят": 9, "десят": 10, "одиннадцат": 11,
	"двенадцат": 12, "двадцат": 20, "тридцат": 30, "полтора": 90,
	"один": 1, "одну": 1, "одна": 1, "два": 2, "две": 2, "три": 3, "четыре": 4,
	"пять": 5, "шесть": 6, "семь": 7, "восьмь": 8, "восемь": 8, "девять": 9, "десять": 10,
}

func parseNumber(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// Direct map check
	if val, ok := ordinalsMap[s]; ok {
		return val
	}
	// Prefix map check
	for k, v := range ordinalsMap {
		if strings.HasPrefix(s, k) {
			return v
		}
	}
	// Extract leading digits: e.g. "5-ю" -> 5, "10-й" -> 10, "30-ю" -> 30
	digits := ""
	for _, r := range s {
		if unicode.IsDigit(r) {
			digits += string(r)
		} else if len(digits) > 0 {
			break
		}
	}
	if digits != "" {
		if val, err := strconv.Atoi(digits); err == nil {
			return val
		}
	}
	return 0
}

func parseSeek(norm string) string {
	// 1. Explicit directional short (e.g. "вперед на 30 секунд", "назад на минуту", "вперед на 5 минут")
	if m := seekDirShortRegex.FindStringSubmatch(norm); len(m) == 4 {
		direction := "forward"
		if m[1] == "назад" {
			direction = "rewind"
		}
		val := parseNumber(m[2])
		unit := m[3]
		sec := 15
		if val > 0 {
			if strings.HasPrefix(unit, "час") {
				sec = val * 3600
			} else if strings.HasPrefix(unit, "сек") {
				sec = val
			} else {
				sec = val * 60
			}
		} else if strings.HasPrefix(unit, "час") {
			sec = 3600
		} else if strings.HasPrefix(unit, "мин") {
			sec = 60
		}
		return fmt.Sprintf("seek:%s:%d", direction, sec)
	}

	// 2. Command with explicit direction (e.g. "перемотай на 2 минуты назад/вперед", "мотни на час назад", "на 5 минут назад")
	if m := seekDirCmdRegex.FindStringSubmatch(norm); len(m) == 4 && m[3] != "" {
		direction := "forward"
		if m[3] == "назад" {
			direction = "rewind"
		}
		val := parseNumber(m[1])
		unit := m[2]
		sec := 15
		if val > 0 {
			if strings.HasPrefix(unit, "час") {
				sec = val * 3600
			} else if strings.HasPrefix(unit, "сек") {
				sec = val
			} else {
				sec = val * 60
			}
		} else if strings.HasPrefix(unit, "час") {
			sec = 3600
		} else if strings.HasPrefix(unit, "мин") {
			sec = 60
		}
		return fmt.Sprintf("seek:%s:%d", direction, sec)
	}

	// 3. Absolute timeline with hours (e.g. "перемотай на 1 час 20 минут", "на 2 часа", "на полтора часа")
	if m := seekHourRegex.FindStringSubmatch(norm); len(m) == 4 {
		hVal := parseNumber(m[1])
		target := hVal * 3600
		if m[1] == "полтора" {
			target = 5400 // 1.5 hours = 90 mins = 5400 sec
		}
		if m[2] != "" {
			target += parseNumber(m[2]) * 60
		}
		if m[3] != "" {
			target += parseNumber(m[3])
		}
		return fmt.Sprintf("seek:to:%d", target)
	}

	// 4. Absolute timeline with composite minutes + seconds (e.g. "поставь на 1 минута 30 секунд", "на 5-ю минуту 30-ю секунду")
	if m := seekMinSecRegex.FindStringSubmatch(norm); len(m) == 3 {
		minVal := parseNumber(m[1])
		secVal := parseNumber(m[2])
		return fmt.Sprintf("seek:to:%d", minVal*60+secVal)
	}

	// 5. Absolute timeline with preposition "с" (e.g. "включи с 10 минуты", "с 10-й минуты")
	if m := seekFromMinRegex.FindStringSubmatch(norm); len(m) == 2 {
		minVal := parseNumber(m[1])
		return fmt.Sprintf("seek:to:%d", minVal*60)
	}

	// 6. Absolute timeline with positioning verbs (e.g. "поставь на 5 минуту", "перейди на 5 минуту")
	if m := seekTimelineActionRegex.FindStringSubmatch(norm); len(m) == 2 {
		minVal := parseNumber(m[1])
		return fmt.Sprintf("seek:to:%d", minVal*60)
	}

	// 7. Absolute timeline with ordinal minutes (e.g. "перемотай на 5-ю минуту", "на пятую минуту")
	if m := seekTimelineOrdinalRegex.FindStringSubmatch(norm); len(m) == 2 {
		minVal := parseNumber(m[1])
		return fmt.Sprintf("seek:to:%d", minVal*60)
	}

	// 8. Clock format timeline (e.g. "перемотай на 12:30", "на 1:45", "на 1:20:00")
	if m := seekClockRegex.FindStringSubmatch(norm); len(m) == 4 {
		var target int
		if m[1] != "" {
			target = parseNumber(m[1])*3600 + parseNumber(m[2])*60 + parseNumber(m[3])
		} else {
			target = parseNumber(m[2])*60 + parseNumber(m[3])
		}
		return fmt.Sprintf("seek:to:%d", target)
	}

	// 9. Absolute seconds timeline (e.g. "перейди на 45 секунду")
	if m := seekSecRegex.FindStringSubmatch(norm); len(m) == 2 {
		secVal := parseNumber(m[1])
		return fmt.Sprintf("seek:to:%d", secVal)
	}

	// 10. Generic relative command (e.g. "перемотай на 5 минут", "перемотай на 5", "на 5", "мотни на минуту", "перемотай на 30 секунд")
	if m := seekSimpleRegex.FindStringSubmatch(norm); len(m) == 3 {
		val := parseNumber(m[1])
		unit := m[2]
		sec := 15
		if val > 0 {
			if strings.HasPrefix(unit, "час") {
				sec = val * 3600
			} else if strings.HasPrefix(unit, "сек") {
				sec = val
			} else {
				// Default to minutes for numbers without units or with "мин": "на 5", "перемотай на 5", "на 5 минут"
				sec = val * 60
			}
		} else if strings.HasPrefix(unit, "час") {
			sec = 3600
		} else if strings.HasPrefix(unit, "мин") {
			sec = 60
		}
		return fmt.Sprintf("seek:forward:%d", sec)
	}

	return ""
}

// SmartTubeMediaCommand recognizes standard media actions (pause, resume, next, etc.) and seek commands.
func SmartTubeMediaCommand(text string) string {
	norm := cleanText(text)
	if norm == "" {
		return ""
	}

	if mediaCloseRegex.MatchString(norm) {
		return "close"
	}
	if mediaPauseRegex.MatchString(norm) {
		return "pause"
	}
	if mediaResumeRegex.MatchString(norm) {
		return "resume"
	}
	if mediaNextRegex.MatchString(norm) {
		return "next"
	}
	if mediaPrevRegex.MatchString(norm) {
		return "previous"
	}
	if mediaRestartRegex.MatchString(norm) {
		return "restart"
	}
	if mediaStatusRegex.MatchString(norm) {
		return "status"
	}
	if seek := parseSeek(norm); seek != "" {
		return seek
	}

	// Fallback to standard playback commands
	if cmd := playback.Command(text); cmd != "" {
		return cmd
	}

	return ""
}

// matchMarkerPattern matches query patterns against action prefixes and app-specific markers.
func matchMarkerPattern(norm string, markers []string, prefixes []string) (string, bool) {
	for _, marker := range markers {
		for _, prefix := range prefixes {
			// 1. "prefix + marker + query"
			lead := prefix + " " + marker + " "
			if strings.HasPrefix(norm, lead) {
				q := strings.TrimSpace(strings.TrimPrefix(norm, lead))
				if q != "" {
					return q, true
				}
			}

			// 2. "prefix + query + marker"
			prefixLead := prefix + " "
			markerTail := " " + marker
			if strings.HasPrefix(norm, prefixLead) && strings.HasSuffix(norm, markerTail) {
				middle := norm[len(prefixLead) : len(norm)-len(markerTail)]
				q := strings.TrimSpace(middle)
				if q != "" {
					return q, true
				}
			}
		}

		// 3. "marker + query"
		markerLead := marker + " "
		if strings.HasPrefix(norm, markerLead) {
			q := strings.TrimSpace(strings.TrimPrefix(norm, markerLead))
			if q != "" {
				return q, true
			}
		}

		// 4. "query + marker"
		markerTail := " " + marker
		if strings.HasSuffix(norm, markerTail) {
			q := strings.TrimSpace(strings.TrimSuffix(norm, markerTail))
			if q != "" && !strings.HasPrefix(q, "пауза") && !strings.HasPrefix(q, "стоп") {
				return q, true
			}
		}
	}
	return "", false
}

// TVAppSearchCommand dispatches voice search queries for LazyMedia Deluxe and Zona.
func TVAppSearchCommand(text string) (string, string, bool) {
	norm := cleanText(text)
	if norm == "" {
		return "", "", false
	}

	// Reject smart home commands (e.g. "включи свет в зоне кухни")
	for _, word := range strings.Fields(norm) {
		for _, hw := range smartHomeFilterPrefixes {
			if strings.HasPrefix(word, hw) {
				return "", "", false
			}
		}
	}

	// Check LazyMedia Deluxe
	if q, ok := parseLazyMediaSearch(norm); ok {
		return "media", q, true
	}

	// Check Zona
	if q, ok := parseZonaSearch(norm); ok {
		return "zona", q, true
	}

	return "", "", false
}

