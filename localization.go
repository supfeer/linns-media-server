package main

import (
	_ "embed"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/lang"
)

//go:embed locales/en.json
var englishTranslations []byte

//go:embed locales/ru.json
var russianTranslations []byte

func initializeLocalization() error {
	locale := lang.SystemLocale()
	return lang.AddTranslationsForLocale(translationsForLocale(locale), locale)
}

func translationsForLocale(locale fyne.Locale) []byte {
	language := strings.ToLower(locale.String())
	if separator := strings.IndexAny(language, "-_"); separator >= 0 {
		language = language[:separator]
	}
	if language == "ru" {
		return russianTranslations
	}
	return englishTranslations
}

func tr(key string) string {
	return lang.X(key, key)
}
