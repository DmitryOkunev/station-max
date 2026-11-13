# Исследование и интеграция голосового управления: Zona и LazyMedia Deluxe

> **Ветка исследования:** `research-tv-apps-voice`  
> **Целевое устройство:** Яндекс Станция Макс (`YNDX-0008` / `YNDX-00052`), Android TV 9  
> **Статус:** Реверс-инжиниринг завершен, механизмы подтверждены на реальном устройстве.

---

## 1. Сравнение с базовой реализацией SmartTube

В базовой настройке SmartTube голосовое управление строится на трех компонентах:
1. **Голосовой парсер (`internal/bridge/smarttube.go`)**:
   - `SmartTubePlayCommand`: выделяет поисковые фразы вида *«включи на ютубе X»*, *«найди в ютубе X»*.
   - `SmartTubeAppCommand`: распознает запуск приложений и навигацию (*«открой ютуб»*, *«открой зону»*, *«домой»*).
   - `SmartTubeMediaCommand`: перехватывает команды плеера (*«пауза»*, *«продолжи»*, *«следующее»*, *«перемотай на N секунд»*).
2. **Исполнитель ADB (`cmd/quasar-bridge/smarttube.go`)**:
   - Запуск через Deep-Link Intent:  
     `am start -a android.intent.action.VIEW -d 'youtube://search?query=<query>&method=play' -n org.smarttube.beta/com.liskovsoft.smartyoutubetv2.tv.ui.main.SplashActivity`
   - Zero-TTS управление через `input keyevent`: 127 (pause), 126 (resume), 87 (next), 88 (prev), 89/90 (seek), 3 (home).
3. **Отслеживание состояния**:
   - Чтение `dumpsys media_session` для проверки состояния воспроизведения (`PlaybackState {state=3}` — playing, `state=2` — paused).

---

## 2. Разбор и результаты для Zona (`mobi.zona`)

### 2.1. Анализ манифеста и Dex-байткода
- **Пакет:** `mobi.zona`
- **Главная Activity:** `ru.zona.app.android.MainActivity`
- **Deep Link Intent Filters:**
  `MainActivity` зарегистрирована как обработчик схем `https://` для хостов:
  - `www.kinopoisk.ru` (пути `/film`, `/series`, `/movies`, `/tvseries`)
  - `.zona.plus`, `.getz.tv`, `.zon.im`, `zonadro.id`, `mobi.zona`
- **Внутренний API поиска:**
  При анализе `classes4.dex` обнаружено, что сама Zona для поиска фильмов использует Kinopoisk Suggest API:
  - **URL:** `https://www.kinopoisk.ru/api/suggest/v2/?query=<название>`
  - **Заголовки:**
    - `User-Agent: Mozilla/5.0 (Windows NT 10.0 Win64 x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/87.0.4280.141 Safari/537.36`
    - `X-Requested-With: XMLHttpRequest`
  - Ответ возвращается за ~80-120мс в чистом JSON:
    ```json
    {
      "suggest": {
        "top": {
          "topResult": {
            "global": {
              "id": 258687,
              "__typename": "Film", // или "TvSeries"
              "title": { "russian": "Интерстеллар" }
            }
          }
        }
      }
    }
    ```

### 2.2. Проверка на живой Станции (Успешно)
1. **Запуск карточки фильма:**
   ```bash
   am start -a android.intent.action.VIEW -d "https://www.kinopoisk.ru/film/258687" -n mobi.zona/ru.zona.app.android.MainActivity
   ```
   **Результат:** Zona мгновенно открывает экран «Интерстеллар (2014)» с кнопкой «Смотреть», рейтингами и описанием.
2. **Автоматический старт воспроизведения:**
   На открытой карточке фильма фокус ввода по умолчанию стоит на кнопке `Смотреть` (`[152,872][472,968]`).
   Посылка `input keyevent 23` (DPAD_CENTER) немедленно запускает воспроизведение видеопотока.
3. **Управление воспроизведением (Zero-TTS):**
   Во время проигрывания Zona регистрирует активный AudioTrack (`CONTENT_TYPE_MOVIE`).
   - Пауза: `input keyevent 127` $\to$ переводит плеер в `state:paused`.
   - Возобновление: `input keyevent 126` $\to$ переводит плеер в `state:started`.
   - Перемотка: `input keyevent 89` / `input keyevent 90`.
   - Выход/Стоп: `input keyevent 127; input keyevent 3; am force-stop mobi.zona`.

---

## 3. Разбор и результаты для LazyMedia Deluxe (`com.lazycatsoftware.lmd`)

### 3.1. Анализ манифеста и Dex-байткода
- **Пакет:** `com.lazycatsoftware.lmd`
- **Поисковая Activity:**  
  `com.lazycatsoftware.lazymediadeluxe.ui.tv.activities.ActivityTvSearch`
- **Прием параметров Intent:**  
  Метод инициализации `ActivityTvSearch` считывает строку `intent.getStringExtra("query")`.
- **Необходимые разрешения:**  
  Поскольку поисковый экран Leanback содержит голосовой поиск, при первом запуске может запрашиваться микрофон. Решение:
  ```bash
  pm grant com.lazycatsoftware.lmd android.permission.RECORD_AUDIO
  ```

### 3.2. Проверка на живой Станции (Успешно)
1. **Запуск поиска по названию:**
   ```bash
   am start -n com.lazycatsoftware.lmd/com.lazycatsoftware.lazymediadeluxe.ui.tv.activities.ActivityTvSearch --es query "Матрица"
   ```
   **Результат:** LazyMedia открывает ТВ-поиск, строка «Матрица» сразу подставляется в запрос, приложение в фоне опрашивает настроенные онлайн-кинотеатры (Rezka, Filmix, КиноПоиск и др.) и выводит ленту карточек.
2. **Фокусировка на первом результате:**
   После открытия экрана посылка:
   ```bash
   input keyevent 20 # Переход с верхней панели на первую найденную карточку
   input keyevent 23 # Открытие карточки фильма (ActivityTvArticle)
   ```
3. **Управление воспроизведением (Zero-TTS):**
   Встроенный плеер LazyMedia (`ActivityExoPlayer`) слушает стандартные медиа-кнопки Android:
   - Пауза: `input keyevent 127`
   - Возобновление: `input keyevent 126`
   - Перемотка: `input keyevent 89` / `input keyevent 90`
   - Закрытие: `input keyevent 127; input keyevent 3; am force-stop com.lazycatsoftware.lmd`

---

## 4. Сводная таблица возможностей интеграции

| Функция | SmartTube | Zona | LazyMedia Deluxe |
| :--- | :--- | :--- | :--- |
| **Пакет** | `org.smarttube.beta` | `mobi.zona` | `com.lazycatsoftware.lmd` |
| **Голосовая команда запуска** | «Открой Ютуб» | «Открой Зону» | «Открой Лейзи» / «Открой Медиа» |
| **Голосовой поиск / Запуск** | «Включи на Ютубе <X>» | «Включи/Найди в Зоне <X>» | «Включи/Найди в Медиа <X>» |
| **Механизм запуска фильма** | Intent `youtube://search?query=X&method=play` | Kinopoisk Suggest API $\to$ `https://www.kinopoisk.ru/film/<ID>` | Intent `ActivityTvSearch` `--es query "X"` |
| **Выбор качества / озвучки** | В плеере SmartTube | На карточке фильма (по умолчанию выбран лучший) | В карточке фильма (выбор балансера/озвучки) |
| **Zero-TTS Пауза/Старт** | `keyevent 127` / `126` | `keyevent 127` / `126` | `keyevent 127` / `126` |
| **Zero-TTS Перемотка** | `keyevent 89` / `90` | `keyevent 89` / `90` | `keyevent 89` / `90` |
| **Zero-TTS Стоп/Домой** | `keyevent 127; 3; force-stop` | `keyevent 127; 3; force-stop` | `keyevent 127; 3; force-stop` |
| **Отслеживание активности** | `dumpsys media_session` | `dumpsys audio` / `media_session` | `dumpsys audio` / `media_session` |

---

## 5. Архитектура интеграции в Quasar Bridge

Чтобы не ломать `main` и сохранить модульность, в ветке `research-tv-apps-voice`:

1. **`internal/bridge/tvapps.go` (расширение голосового парсера):**
   - Добавляются шаблоны для Zona:
     - `^(?:включи|найди|открой|поставь)(?: в| на)? зоне (.+)$` $\to$ `zona_play` / `zona_search`
   - Добавляются шаблоны для LazyMedia:
     - `^(?:включи|найди|открой|поставь)(?: в| на)? (?:медиа|лейзи|lazy|lazymedia) (.+)$` $\to$ `lazy_search`
2. **`cmd/quasar-bridge/tvapps.go` (контроллер медиа-приложений):**
   - Функция `ResolveKinopoiskID(ctx, query) (id int, isSeries bool, title string, err error)`:
     выполняет быстрый запрос к suggest API.
   - Запуск Zona:
     Формирует deep-link URL (`https://www.kinopoisk.ru/film/<id>` или `/series/<id>`), отправляет intent в `ru.zona.app.android.MainActivity`.
   - Запуск LazyMedia:
     Отправляет intent в `ActivityTvSearch` с параметром `--es query`.
   - Универсальный трекер активности:
     Определяет, какое из трех приложений (`org.smarttube.beta`, `mobi.zona`, `com.lazycatsoftware.lmd`) сейчас активно через `dumpsys media_session` / `dumpsys audio`, и перенаправляет общие команды («Алиса, пауза», «Алиса, дальше», «Алиса, перемотай на минуту») в активный плеер.
