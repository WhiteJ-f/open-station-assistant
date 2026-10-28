package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

type WeatherService struct {
	client    *http.Client
	city      string
	latitude  float64
	longitude float64
}

func NewWeatherService(city string, lat, lon float64) *WeatherService {
	return &WeatherService{
		client:    &http.Client{Timeout: 5 * time.Second},
		city:      city,
		latitude:  lat,
		longitude: lon,
	}
}

type openMeteoResponse struct {
	Current struct {
		Temperature2m float64 `json:"temperature_2m"`
		ApparentTemp  float64 `json:"apparent_temperature"`
		Humidity      int     `json:"relative_humidity_2m"`
		WeatherCode   int     `json:"weather_code"`
		WindSpeed     float64 `json:"wind_speed_10m"`
	} `json:"current"`
}

func (ws *WeatherService) GetCurrentWeather() (string, error) {
	url := fmt.Sprintf("https://api.open-meteo.com/v1/forecast?latitude=%.4f&longitude=%.4f&current=temperature_2m,apparent_temperature,relative_humidity_2m,weather_code,wind_speed_10m&wind_speed_unit=ms",
		ws.latitude, ws.longitude)

	resp, err := ws.client.Get(url)
	if err != nil {
		return "", fmt.Errorf("weather request error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("weather service returned HTTP %d", resp.StatusCode)
	}

	var data openMeteoResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", fmt.Errorf("weather decode error: %w", err)
	}

	temp := int(math.Round(data.Current.Temperature2m))
	wind := int(math.Round(data.Current.WindSpeed))
	condition := decodeWeatherCode(data.Current.WeatherCode)

	var tempStr string
	if temp > 0 {
		tempStr = fmt.Sprintf("плюс %d", temp)
	} else if temp < 0 {
		tempStr = fmt.Sprintf("минус %d", -temp)
	} else {
		tempStr = "ноль"
	}

	degreeWord := pluralize(int(math.Abs(float64(temp))), "градус", "градуса", "градусов")
	windWord := pluralize(wind, "метр", "метра", "метров")

	return fmt.Sprintf("Сейчас в городе %s %s, температура %s %s, ветер %d %s в секунду.",
		ws.city, condition, tempStr, degreeWord, wind, windWord), nil
}

func decodeWeatherCode(code int) string {
	switch code {
	case 0:
		return "ясно"
	case 1:
		return "в основном ясно"
	case 2:
		return "переменная облачность"
	case 3:
		return "пасмурно"
	case 45, 48:
		return "туманно"
	case 51, 53, 55:
		return "небольшая морось"
	case 61:
		return "небольшой дождь"
	case 63:
		return "дождь"
	case 65:
		return "сильный дождь"
	case 71, 73, 75:
		return "снегопад"
	case 80, 81, 82:
		return "ливневый дождь"
	case 85, 86:
		return "ливневый снег"
	case 95, 96, 99:
		return "гроза"
	default:
		return "облачно"
	}
}

func pluralize(n int, one, two, five string) string {
	n100 := n % 100
	if n100 >= 11 && n100 <= 19 {
		return five
	}
	n10 := n % 10
	if n10 == 1 {
		return one
	}
	if n10 >= 2 && n10 <= 4 {
		return two
	}
	return five
}
