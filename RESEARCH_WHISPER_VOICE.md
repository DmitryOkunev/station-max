# Исследование и архитектура: Детекция шепота и синтез ответов шепотом

> **Статус:** Реализовано и развернуто в продакшене (`internal/bridge/whisper_detect.go`, `native.go`).  
> **Целевая платформа:** Яндекс Станция Макс (`YNDX-0008` / `YNDX-00052`), Quasar Bridge (`internal/bridge`).  
> **Ключевой вывод:** Детектор шепота работает в реальном времени на входящем потоке Ogg Opus через `pion/opus` (16 кГц PCM) и DSP-анализ (RMS, ZCR, Autocorrelation, Spectral Tilt). Для синтеза шепота в Yandex SpeechKit используется SSML-тег `<speaker is_whisper="true">` в тексте речи (`ttsGen.Text`, `VoiceResponse.OutputSpeech.Text`, `TBaseResponse.Voice`). Поле `Emotion = "whisper"` игнорируется API Яндекса (допустимые значения эмоций: `neutral`, `good`, `evil`). При обнаружении шепота тег добавляется к речи, и облако Яндекса синтезирует оригинальный нейросетевой шепот Алисы (`shitova`).

---

## 1. Как устроен шепот в штатной экосистеме Яндекса

При перехвате живого трафика голосовых запросов шепотом на Станции (`Vins.VoiceInput` $\to$ `AsrResult` $\to$ `TtsSpeak`) выявлена следующая цепочка:

```
[Колонка] (Opus аудио)
   │
   ▼
[UniProxy / ASR] ───────────────► Распознавание текста + Акустический классификатор
   │                              (выставляет флаг AsrWhisper в Megamind TEvent)
   ▼
[Megamind / Сценарий Алисы] ────► При AsrWhisper=true активирует стиль шепота
   │
   ▼
[Yandex SpeechKit TTS] ─────────► Синтез речи с role="whisper" (emotion="whisper")
   │                              голосом shitova (нейросетевой шепот)
   ▼
[Колонка] ◄────────────────────── Стриминг аудио Opus (TtsSpeak)
```

### Важные технические факты:
1. **Колонка не применяет DSP-фильтров для шепота:** Звук синтезируется шепотом непосредственно нейросетевым вокодером в облаке Яндекса. Колонка получает стандартные Opus-пакеты и просто воспроизводит их через ЦАП.
2. **Внутренние Protobuf-поля Яндекса:**
   - `proto/alice/megamind/protos/common/events.proto`:
     Поле `AsrWhisper *bool = 16` в структуре `TEvent`.
   - `proto/alice/protos/api/alicekit/scenarios/protocol/request.proto`:
     Служебный источник данных `WhisperInfo` в `TDataSource`.
3. **Ограничение публичного `AsrResult`:**
   В клиентском потоке WebSocket (`f.Kind() == "AsrResult"`) UniProxy возвращает только гипотезы текста (`words`, `normalized`, `confidence`). Флаг `AsrWhisper` остается внутри облачного пайплайна Megamind. Так как Quasar Bridge при перехвате на LLM отменяет Megamind (`sendCancel`), мост не получает флаг шепота от серверов Яндекса.

---

## 2. Архитектура решения в Quasar Bridge

Так как генерация TTS для сторонних LLM в Quasar Bridge уже выполняется через UniProxy, решение состоит из двух изолированных частей:
1. **Локальный детектор шепота** во входящем аудиопотоке моста (`isWhisper: true/false`).
2. **Передача флага в облачный синтез `TTtsGenerate`** при генерации ответа.

```
                           ┌────────────────────────────────────────┐
                           │          Quasar Bridge Core            │
                           └──────────────────┬─────────────────────┘
                                              │
              ┌───────────────────────────────┴───────────────────────────────┐
              ▼                                                               ▼
 ┌─────────────────────────┐                                     ┌─────────────────────────┐
 │ Входящий поток (клиент) │                                     │  Генерация TTS (выход)  │
 ├─────────────────────────┤                                     ├─────────────────────────┤
 │ 1. Аудио Opus (opaque)  │ ──► [Детектор шепота (DSP/ONNX)]    │ 1. LLM вернула текст    │
 │ 2. ASR через UniProxy   │         │ (isWhisper = true)        │ 2. synthesize()         │
 │ 3. Получен текст        │         ▼                           │ 3. TTtsGenerate {       │
 │ 4. Маршрутизация на LLM │ ──► [Флаг запроса: p.isWhisper] ──► │      Text: text,        │
 └─────────────────────────┘                                     │      Emotion: "whisper" │
                                                                 │    }                    │
                                                                 └────────────┬────────────┘
                                                                              │
                                                                              ▼
                                                                  [Яндекс UniProxy SpeechKit]
                                                                  (синтезирует шепот Алисы)
```

---

## 3. Спецификация генерации TTS шепотом (`TTtsGenerate` & SSML)

В Yandex SpeechKit/UniProxy для голоса `shitova` поле `Emotion` принимает только значения `"neutral"`, `"good"` и `"evil"`. Значение `"whisper"` в `Emotion` игнорируется движком синтеза.
Нейросетевой шёпот Алисы активируется исключительно через SSML-тег:
```xml
<speaker is_whisper="true"> Текст ответа...
```

Данный формат был подтвержден прямым дампом ответов Яндекса из `conversations_audio.jsonl`:
```json
{"event":"cloud_response","kind":"AliceResponse","text":"Я бы с радостью...","speech":"<speaker is_whisper=\"true\"> Я бы с радостью..."}
```

### Реализация в Quasar Bridge (`internal/bridge/native.go`):

При генерации речи в `synthesize` тег добавляется в начало `ttsGen.Text`:

```go
func (n *Native) synthesize(p *nativeRequest, text string) error {
	text = n.prepareTTSText(text)
	ttsText := text
	if p.isWhisper || (p.detector != nil && p.detector.IsWhisper()) {
		p.isWhisper = true
		if !strings.HasPrefix(ttsText, "<speaker is_whisper=") {
			ttsText = "<speaker is_whisper=\"true\"> " + ttsText
		}
		slog.Info("native_tts_whisper_activated", "request", requestTag(p.id))
	}
	ttsGen := &tts.TTtsGenerate{
		Text:   new(ttsText),
		Lang:   new("ru-RU"),
		Format: new("audio/opus"),
	}
	if p.rule == "ask_llm_slysh" {
		evil := "evil"
		ttsGen.Emotion = &evil
		slog.Info("native_tts_emotion_evil", "request", requestTag(p.id))
	}
...
```

Аналогично тег проставляется в `VoiceResponse.OutputSpeech.Text` (для `AliceResponse`) и в `voice` (`TDeferedAliceResponseProto_TBaseResponse` для стриминга). На экраны (карточки `Cards` и `base.Text`) тег **не** выводится, сохраняя чистый читаемый текст.

---

## 4. Детекция шепота во входящем аудио (варианты реализации)

Поскольку UniProxy не отдает наружу флаг `AsrWhisper`, мост анализирует первые 300–600 мс входящего аудиопотока (`opaque` Opus-фреймы от колонки).

### Физические свойства шепота:
* **Отсутствие частоты основного тона ($F_0$ / Pitch):** связки разомкнуты, нет периодических импульсов в диапазоне 80–350 Гц.
* **Высокий Spectral Tilt:** энергия смещена вверх, в диапазон 2–5 кГц.
* **Низкий HNR (Harmonic-to-Noise Ratio):** гармоническая составляющая близка к 0.

### Вариант 1: Легковесный DSP-анализ (Рекомендуемый, 0 GPU, <2 мс)
1. Декодирование первых 300 мс аудио Opus в PCM 16 кГц (`gopus` или `hrpc/opus`).
2. Оценка энергии фрейма: если RMS ниже порога тишины — пропуск.
3. Проверка автокорреляции: поиск пиков основного тона ($F_0$).
4. Отношение энергии спектра:
   $$R = \frac{E_{2000-6000\,\text{Hz}}}{E_{100-1000\,\text{Hz}}}$$
5. Если $F_0$ не обнаружен и $R > 1.8$, выставляется `p.isWhisper = true`.

### Вариант 2: ONNX Audio Classifier (~2 МБ, 5 мс)
1. Снятие мел-спектрограммы 300 мс фрагмента.
2. Прогон через компактную бинарную модель (например, обученная MobileNetV3 / 1D-CNN на датасете Whispered TIMIT / AudioSet).
3. При `prob(whisper) > 0.75` выставляется `p.isWhisper = true`.

---

## 5. Заметки на будущее (LLM Промпты)

> [!NOTE]
> **Промпты для LLM на текущем этапе намеренно не модифицируются.**  
> В будущем при полной активации детектора шепота целесообразно добавить динамический контекстный модификатор:
>
> ```
> [Системное указание при p.isWhisper == true]:
> «Пользователь говорит шепотом. Отвечай предельно тихо, кратко и лаконично (не более 1–2 коротких предложений). Без развернутых списков и длинных пояснений.»
> ```
> *Причина:* Длинный синтез шепотом утомляет слух и не соответствует ночным/тихим сценариям использования умной колонки.

