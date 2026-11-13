package bridge

import (
	"regexp"
	"strings"
	"unicode"
)

type ttsPatternRule struct {
	re   *regexp.Regexp
	repl string
}

// TTSReplacer unmasks *-censored Russian profanity before sending text to TTS.
type TTSReplacer struct {
	exact map[string]string
}

var (
	ttsTokenRegex = regexp.MustCompile(`(?:[а-яА-ЯёЁ*]|\\\*)+`)

	// Default exact lookup matrix (lowercase masked token -> unmasked Russian word).
	defaultTTSExact = map[string]string{
		"бл*":       "бля",
		"бл**":      "бля",
		"бл*дь":     "блядь",
		"бл*ть":     "блять",
		"п*здец":    "пиздец",
		"п*зда":     "пизда",
		"п*здак":    "пиздак",
		"п*здишь":   "пиздишь",
		"п*здит":    "пиздит",
		"п*здят":    "пиздят",
		"п*здеть":   "пиздеть",
		"п*здел":    "пиздел",
		"п*здеж":    "пиздеж",
		"п*здёж":    "пиздёж",
		"п*здабол":  "пиздабол",
		"п*здюк":    "пиздюк",
		"х*р":       "хер",
		"х*рня":     "херня",
		"х*рни":     "херни",
		"х*рню":     "херню",
		"х*рней":    "херней",
		"х*рнёй":    "хернёй",
		"нах*р":     "нахер",
		"пох*р":     "похер",
		"них*ра":    "нихера",
		"х*й":       "хуй",
		"х*йня":     "хуйня",
		"х*йни":     "хуйни",
		"х*йню":     "хуйню",
		"нах*й":     "нахуй",
		"пох*й":     "похуй",
		"них*я":     "нихуя",
		"ох*еть":    "охуеть",
		"ох*ел":     "охуел",
		"ах*еть":    "ахуеть",
		"еб*ть":     "ебать",
		"еб*ный":    "ебаный",
		"еб*нутый":  "ебанутый",
		"еб*ло":     "ебало",
		"еб*льник":  "ебальник",
		"заеб*л":    "заебал",
		"заеб*ла":   "заебала",
		"заеб*ло":   "заебало",
		"заеб*сь":   "заебись",
		"отъеб*сь":  "отъебись",
		"съеб*сь":   "съебись",
		"долбо*б":   "долбоёб",
		"уеб*к":     "уёбок",
		"с*ка":      "сука",
		"с*ки":      "суки",
		"с*чка":     "сучка",
		"м*дак":     "мудак",
		"м*даки":    "мудаки",
		"м*дила":    "мудила",
		"г*вно":     "говно",
		"ж*па":      "жопа",
		"п*дор":     "пидор",
		"п*дорас":   "пидорас",
		"г*ндон":    "гандон",
		"шл*ха":     "шлюха",
		"мр*зь":     "мразь",
	}

	// Morphological stem rules for any *-count and Russian inflection.
	defaultTTSPatterns = []ttsPatternRule{
		// бля / блядь / блять family
		{regexp.MustCompile(`^бл\*+$`), "бля"},
		{regexp.MustCompile(`^б\*{2,}$`), "бля"},
		{regexp.MustCompile(`^б\*+я$`), "бля"},
		{regexp.MustCompile(`^б\*+я([дт]ь[а-яё]*)$`), "бля$1"},
		{regexp.MustCompile(`^б\*+([дт]ь)$`), "бля$1"},
		{regexp.MustCompile(`^бля\*+$`), "блять"},
		{regexp.MustCompile(`^бляд\*+$`), "блядь"},
		{regexp.MustCompile(`^бл\*+([дт]ь|д[ауеоы][а-яё]*|дск[а-яё]+|дств[а-яё]*|дот[а-яё]*|дин[а-яё]*)$`), "бля$1"},
		{regexp.MustCompile(`^(вы|про|раз|от|под|на|по|у|пере)бл\*+(д[а-яё]*)$`), "${1}бля$2"},
		{regexp.MustCompile(`^б\*+я\*+$`), "блять"},

		// пизд* family
		{regexp.MustCompile(`^([а-яё]*)п\*+зд\*+ц$`), "${1}пиздец"},
		{regexp.MustCompile(`^([а-яё]*)п\*+з\*+ц$`), "${1}пиздец"},
		{regexp.MustCompile(`^([а-яё]*)п\*+зд\*+бол([а-яё]*)$`), "${1}пиздабол$2"},
		{regexp.MustCompile(`^([а-яё]*)п\*+зд([а-яё]+)$`), "${1}пизд$2"},
		{regexp.MustCompile(`^([а-яё]*)п\*+[з*]*дец$`), "${1}пиздец"},
		{regexp.MustCompile(`^([а-яё]*)п\*+[з*]*дак([а-яё]*)$`), "${1}пиздак$2"},
		{regexp.MustCompile(`^([а-яё]*)п\*+[з*]*дишь$`), "${1}пиздишь"},
		{regexp.MustCompile(`^([а-яё]*)п\*+[з*]*дит([а-яё]*)$`), "${1}пиздит$2"},
		{regexp.MustCompile(`^([а-яё]*)п\*+[з*]*деть$`), "${1}пиздеть"},
		{regexp.MustCompile(`^([а-яё]*)п\*+ец$`), "${1}пиздец"},
		{regexp.MustCompile(`^([а-яё]*)пи\*+д([а-яё]*)$`), "${1}пизд$2"},
		{regexp.MustCompile(`^([а-яё]*)пиз\*+([а-яё]*)$`), "${1}пизд$2"},
		{regexp.MustCompile(`^([а-яё]*)пизд\*+ж([а-яё]*)$`), "${1}пиздёж$2"},
		{regexp.MustCompile(`^([а-яё]*)пизд\*+шь$`), "${1}пиздишь"},
		{regexp.MustCompile(`^([а-яё]*)пизд\*+бол([а-яё]*)$`), "${1}пиздабол$2"},
		{regexp.MustCompile(`^([а-яё]*)пизд\*+к([а-яё]*)$`), "${1}пиздюк$2"},
		{regexp.MustCompile(`^([а-яё]*)пизд\*+$`), "${1}пизда"},

		// хер / хуй family
		{regexp.MustCompile(`^([а-яё]*)х\*+р\*+([аяоеиуыю])$`), "${1}херн$2"},
		{regexp.MustCompile(`^([а-яё]*)х\*+р([а-яё]*)$`), "${1}хер$2"},
		{regexp.MustCompile(`^([а-яё]*)хе\*+н([яиюеё][а-яё]*)$`), "${1}херн$2"},
		{regexp.MustCompile(`^([а-яё]*)х\*+(й[а-яё]*)$`), "${1}ху$2"},
		{regexp.MustCompile(`^([а-яё]*)х\*+([яюи])$`), "${1}ху$2"},
		{regexp.MustCompile(`^([а-яё]*)х\*+([яю][а-яё]+)$`), "${1}ху$2"},
		{regexp.MustCompile(`^([а-яё]*)х\*+([её][а-яё]*)$`), "${1}ху$2"},
		{regexp.MustCompile(`^([а-яё]*[оа])\*+у([её][а-яё]*)$`), "${1}ху$2"},
		{regexp.MustCompile(`^([а-яё]*)ху\*+н([а-яё]+)$`), "${1}хуйн$2"},
		{regexp.MustCompile(`^([а-яё]*)ху\*+([яю][а-яё]*)$`), "${1}ху$2"},
		{regexp.MustCompile(`^([а-яё]*)ху\*+$`), "${1}хуй"},
		{regexp.MustCompile(`^(на|по|за)х\*+$`), "${1}хуй"},
		{regexp.MustCompile(`^х\*{2,}н([а-яё]+)$`), "хуйн$1"},
		{regexp.MustCompile(`^х\*{2,}$`), "хуй"},

		// еб / ёб family
		{regexp.MustCompile(`^([а-яё]*)уеб\*+к$`), "${1}уёбок"},
		{regexp.MustCompile(`^([а-яё]*)уеб\*+к([ауеиы][а-яё]*)$`), "${1}уёбк$2"},
		{regexp.MustCompile(`^([а-яё]*)уеб\*+щ([а-яё]+)$`), "${1}уёбищ$2"},
		{regexp.MustCompile(`^([а-яё]*)долбо\*+б([а-яё]*)$`), "${1}долбоёб$2"},
		{regexp.MustCompile(`^д\*+лбо[её]б([а-яё]*)$`), "долбоёб$1"},
		{regexp.MustCompile(`^долб\*+[её]б([а-яё]*)$`), "долбоёб$1"},
		{regexp.MustCompile(`^([а-яё]*)еб\*+(т|тся|шь|м|те)$`), "${1}ебё$2"},
		{regexp.MustCompile(`^([а-яё]*)еб\*+(с[ьт][а-яё]*)$`), "${1}еби$2"},
		{regexp.MustCompile(`^([а-яё]*)еб\*+(ть|ться|л|ла|ли|ло|лся|лась|лись|лось|ль[а-яё]*|н[а-яё]*|к[а-яё]*|ч[а-яё]+|ш[иуае][а-яё]*)$`), "${1}еба$2"},
		{regexp.MustCompile(`^([а-яё]*)ёб\*+(ну[а-яё]+)$`), "${1}ёб$2"},
		{regexp.MustCompile(`^([а-яё]*)ёб\*+(ть|ться|л|ла|ли|ло|н[а-яё]*)$`), "${1}ёба$2"},
		{regexp.MustCompile(`^([а-яё]*)е\*+а(ть|ться|л[а-яё]*|н[а-яё]*)$`), "${1}еба$2"},
		{regexp.MustCompile(`^([а-яё]*)е\*+ть$`), "${1}ебать"},
		{regexp.MustCompile(`^([а-яё]*)про\*+б([а-яё]*)$`), "${1}проёб$2"},
		{regexp.MustCompile(`^([а-яё]*)\*+ба(ть|ться|л[а-яё]*|н[а-яё]*|ш[а-яё]*)$`), "${1}еба$2"},
		{regexp.MustCompile(`^([а-яё]*)\*+б(у|ет|ёт|ёшь|ешь|ут)$`), "${1}еб$2"},
		{regexp.MustCompile(`^([а-яё]*)\*+быва([а-яё]+)$`), "${1}ебыва$2"},
		{regexp.MustCompile(`^([а-яё]*)\*+бон([а-яё]*)$`), "${1}ебон$2"},
		{regexp.MustCompile(`^([а-яё]*)\*+би(сь|те|тесь)$`), "${1}еби$2"},

		// сука / мудак / говно / жопа / пидор / прочие
		{regexp.MustCompile(`^с\*+к([аиуое][а-яё]*)$`), "сук$1"},
		{regexp.MustCompile(`^с\*+к\*+$`), "сука"},
		{regexp.MustCompile(`^с\*+ч(к[а-яё]+|ар[а-яё]+|ий|ья|ье|ьи)$`), "суч$1"},
		{regexp.MustCompile(`^м\*+д(ак[а-яё]*|ил[а-яё]*|ач[а-яё]*|азвон[а-яё]*)$`), "муд$1"},
		{regexp.MustCompile(`^г\*+вн([а-яё]+)$`), "говн$1"},
		{regexp.MustCompile(`^г\*+в\*+о$`), "говно"},
		{regexp.MustCompile(`^ж\*+п([а-яё]+)$`), "жоп$1"},
		{regexp.MustCompile(`^п\*+д(ор[а-яё]*|ар[а-яё]*|рил[а-яё]*)$`), "пид$1"},
		{regexp.MustCompile(`^г\*+нд([оа]н[а-яё]*)$`), "ганд$1"},
		{regexp.MustCompile(`^шл\*+х([а-яё]*)$`), "шлюх$1"},
		{regexp.MustCompile(`^з\*+луп([а-яё]*)$`), "залуп$1"},
		{regexp.MustCompile(`^д\*+бил([а-яё]*)$`), "дебил$1"},
		{regexp.MustCompile(`^мр\*+з([а-яё]*)$`), "мраз$1"},
		{regexp.MustCompile(`^тв\*+р([а-яё]*)$`), "твар$1"},
		{regexp.MustCompile(`^чм\*+$`), "чмо"},
		{regexp.MustCompile(`^л\*+х([а-яё]*)$`), "лох$1"},
	}

	defaultTTSReplacer = NewTTSReplacer(nil)
)

// NewTTSReplacer creates a replacer combining custom matrix overrides with built-in defaults.
func NewTTSReplacer(overrides map[string]string) *TTSReplacer {
	exact := make(map[string]string, len(defaultTTSExact)+len(overrides))
	for k, v := range defaultTTSExact {
		exact[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	for k, v := range overrides {
		key := strings.ToLower(strings.TrimSpace(k))
		val := strings.TrimSpace(v)
		if key != "" && val != "" {
			exact[key] = val
		}
	}
	return &TTSReplacer{exact: exact}
}

// Replace expands *-censored Russian swear words into full words before TTS synthesis.
func (r *TTSReplacer) Replace(text string) string {
	if !strings.ContainsRune(text, '*') {
		return text
	}
	return ttsTokenRegex.ReplaceAllStringFunc(text, func(token string) string {
		clean := strings.ReplaceAll(token, `\*`, `*`)
		if !strings.ContainsRune(clean, '*') {
			return token
		}
		if replaced, ok := r.replaceToken(clean); ok {
			return matchTokenCase(clean, replaced)
		}
		// Handle markdown-wrapped tokens like *п*здец* or **бл***
		if strings.HasPrefix(clean, "*") && strings.HasSuffix(clean, "*") && len(clean) > 2 {
			trimmed := strings.TrimPrefix(clean, "*")
			trimmed = strings.TrimSuffix(trimmed, "*")
			if trimmed != "" && strings.ContainsRune(trimmed, '*') {
				if replaced, ok := r.replaceToken(trimmed); ok {
					return matchTokenCase(trimmed, replaced)
				}
			}
			// Try stripping balanced **...**
			if strings.HasPrefix(clean, "**") && strings.HasSuffix(clean, "**") && len(clean) > 4 {
				trimmed2 := clean[2 : len(clean)-2]
				if strings.ContainsRune(trimmed2, '*') {
					if replaced, ok := r.replaceToken(trimmed2); ok {
						return matchTokenCase(trimmed2, replaced)
					}
				}
			}
		}
		return token
	})
}

func (r *TTSReplacer) replaceToken(token string) (string, bool) {
	lower := strings.ToLower(token)
	if r != nil && len(r.exact) > 0 {
		if v, ok := r.exact[lower]; ok {
			return v, true
		}
	}
	for _, rule := range defaultTTSPatterns {
		if rule.re.MatchString(lower) {
			return rule.re.ReplaceAllString(lower, rule.repl), true
		}
	}
	return "", false
}

func matchTokenCase(orig, repl string) string {
	if repl == "" {
		return repl
	}
	var letters, upper int
	var firstLetterUpper bool
	firstSeen := false
	for _, ch := range orig {
		if unicode.IsLetter(ch) {
			letters++
			if unicode.IsUpper(ch) {
				upper++
				if !firstSeen {
					firstLetterUpper = true
				}
			}
			firstSeen = true
		}
	}
	if letters > 1 && letters == upper {
		return strings.ToUpper(repl)
	}
	if firstLetterUpper {
		runes := []rune(repl)
		runes[0] = unicode.ToUpper(runes[0])
		return string(runes)
	}
	return repl
}
