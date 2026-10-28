package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type IntentEngine struct {
	weather *WeatherService
	player  *AudioPlayer
	leds    *LEDManager
	cfg     *Config
}

func NewIntentEngine(cfg *Config, player *AudioPlayer, leds *LEDManager, weather *WeatherService) *IntentEngine {
	return &IntentEngine{
		weather: weather,
		player:  player,
		leds:    leds,
		cfg:     cfg,
	}
}

type IntentResult struct {
	Handled bool
	Answer  string
	Action  string
}

// TryHandle matches traditional Alice deterministic commands.
// Returns Handled = true if resolved locally with zero LLM usage.
func (ie *IntentEngine) TryHandle(text string) (IntentResult, error) {
	norm := strings.ToLower(text)
	norm = strings.Trim(norm, " .,!?;:-_«»\"'…")

	loc := time.FixedZone("MSK", ie.cfg.TimezoneOffsetHours*3600)

	// 1. Time query
	if norm == "сколько времени" || norm == "который час" || norm == "время" ||
		strings.Contains(norm, "сколько сейчас времени") || strings.Contains(norm, "подскажи время") ||
		strings.Contains(norm, "скажи время") || strings.Contains(norm, "точное время") {
		now := time.Now().In(loc)
		hour := now.Hour()
		minute := now.Minute()
		hourWord := pluralize(hour, "час", "часа", "часов")
		minWord := pluralize(minute, "минута", "минуты", "минут")
		if minute == 0 {
			return IntentResult{Handled: true, Answer: fmt.Sprintf("Сейчас ровно %d %s.", hour, hourWord)}, nil
		}
		return IntentResult{Handled: true, Answer: fmt.Sprintf("Сейчас %d %s %d %s.", hour, hourWord, minute, minWord)}, nil
	}

	// 2. Date query
	if norm == "какое сегодня число" || norm == "какое число" || norm == "какой сегодня день" ||
		strings.Contains(norm, "какая сегодня дата") || strings.Contains(norm, "скажи дату") {
		now := time.Now().In(loc)
		day := now.Day()
		monthNames := []string{
			"", "января", "февраля", "марта", "апреля", "мая", "июня",
			"июля", "августа", "сентября", "октября", "ноября", "декабря",
		}
		weekdays := map[time.Weekday]string{
			time.Monday:    "понедельник",
			time.Tuesday:   "вторник",
			time.Wednesday: "среда",
			time.Thursday:  "четверг",
			time.Friday:    "пятница",
			time.Saturday:  "суббота",
			time.Sunday:    "воскресенье",
		}
		monthName := monthNames[int(now.Month())]
		weekdayName := weekdays[now.Weekday()]
		return IntentResult{
			Handled: true,
			Answer:  fmt.Sprintf("Сегодня %s, %d %s %d года.", weekdayName, day, monthName, now.Year()),
		}, nil
	}

	// 3. Voice Volume Control
	if matched, newVol := ie.checkVolume(norm); matched {
		ie.player.SetVolume(newVol)
		ie.leds.ShowVolume(newVol)
		ie.player.PlayVolumeClick()
		return IntentResult{
			Handled: true,
			Action:  "VOLUME",
			Answer:  fmt.Sprintf("Громкость: %d%%.", newVol),
		}, nil
	}

	// 4. Playback / Stop / Dismissal Commands
	if norm == "стоп" || norm == "хватит" || norm == "пауза" || norm == "останови" ||
		norm == "замолчи" || norm == "выключись" || norm == "тихо" || norm == "молчи" ||
		norm == "отмена" || norm == "прекрати" || norm == "спасибо" || norm == "все спасибо" ||
		norm == "пока" || norm == "все" || norm == "отбой" || norm == "выключи" || norm == "и выключи" ||
		norm == "stop" || norm == "cancel" ||
		strings.Contains(norm, "останови музыку") || strings.Contains(norm, "выключи музыку") ||
		strings.Contains(norm, "поставь на паузу") || strings.Contains(norm, "хватит болтать") ||
		strings.Contains(norm, "выключи колонку") {
		ie.player.Stop()
		ie.player.PlayCancel()
		ie.leds.SetMode(ModeIdle)
		return IntentResult{
			Handled: true,
			Action:  "STOP",
			Answer:  "",
		}, nil
	}

	// 5. Weather query
	if strings.Contains(norm, "погода") || strings.Contains(norm, "погоду") ||
		strings.Contains(norm, "прогноз") || strings.Contains(norm, "что на улице") ||
		strings.Contains(norm, "температура на улице") {
		ans, err := ie.weather.GetCurrentWeather()
		if err != nil {
			return IntentResult{Handled: true, Answer: "Не удалось получить прогноз погоды."}, nil
		}
		return IntentResult{Handled: true, Answer: ans}, nil
	}

	return IntentResult{Handled: false}, nil
}

func (ie *IntentEngine) checkVolume(norm string) (bool, int) {
	currentVol := ie.player.GetVolume()

	// Relative decrease
	if norm == "тише" || norm == "потише" || norm == "сделай тише" || norm == "убавь" ||
		norm == "убавь звук" || norm == "чуть тише" || norm == "немного тише" ||
		strings.HasSuffix(norm, "тише") || strings.HasSuffix(norm, "потише") {
		newVol := currentVol - 10
		if newVol < 5 {
			newVol = 5
		}
		return true, newVol
	}

	// Relative increase
	if norm == "громче" || norm == "погромче" || norm == "сделай громче" || norm == "прибавь" ||
		norm == "прибавь звук" || norm == "чуть громче" || norm == "немного громче" ||
		strings.HasSuffix(norm, "громче") || strings.HasSuffix(norm, "погромче") {
		newVol := currentVol + 10
		if newVol > 100 {
			newVol = 100
		}
		return true, newVol
	}

	// Maximum
	if strings.Contains(norm, "на максимум") || strings.Contains(norm, "на полную") ||
		strings.Contains(norm, "максимальная громкость") || strings.Contains(norm, "громкость на максимум") {
		return true, 100
	}

	// Minimum
	if strings.Contains(norm, "на минимум") || strings.Contains(norm, "минимальная громкость") ||
		strings.Contains(norm, "громкость на минимум") {
		return true, 5
	}

	// Absolute volume (e.g. "громкость 5", "громкость 50", "звук на 7", "громкость на 3")
	reVol := regexp.MustCompile(`(?:громкость|звук)(?:\s+на)?\s+(\d+)`)
	if m := reVol.FindStringSubmatch(norm); len(m) > 1 {
		val, err := strconv.Atoi(m[1])
		if err == nil {
			if val <= 10 {
				val = val * 10
			}
			if val < 5 {
				val = 5
			}
			if val > 100 {
				val = 100
			}
			return true, val
		}
	}

	wordNumbers := map[string]int{
		"ноль": 5, "один": 10, "два": 20, "три": 30, "четыре": 40,
		"пять": 50, "шесть": 60, "семь": 70, "восемь": 80, "девять": 90, "десять": 100,
	}
	for word, v := range wordNumbers {
		if strings.Contains(norm, "громкость "+word) || strings.Contains(norm, "звук "+word) ||
			strings.Contains(norm, "громкость на "+word) {
			return true, v
		}
	}

	return false, currentVol
}


