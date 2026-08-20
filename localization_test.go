package main

import (
	"encoding/json"
	"testing"

	"fyne.io/fyne/v2"
)

func TestLocalizationCatalogsMatch(t *testing.T) {
	catalogs := map[string][]byte{"en": englishTranslations, "ru": russianTranslations}
	decoded := make(map[string]map[string]string, len(catalogs))
	for language, data := range catalogs {
		messages := map[string]string{}
		if err := json.Unmarshal(data, &messages); err != nil {
			t.Fatalf("decode %s catalog: %v", language, err)
		}
		for key, value := range messages {
			if value == "" {
				t.Fatalf("%s catalog has empty value for %q", language, key)
			}
		}
		decoded[language] = messages
	}
	for key := range decoded["en"] {
		if _, ok := decoded["ru"][key]; !ok {
			t.Errorf("Russian catalog is missing %q", key)
		}
	}
	for key := range decoded["ru"] {
		if _, ok := decoded["en"][key]; !ok {
			t.Errorf("English catalog is missing %q", key)
		}
	}
	for _, locale := range []fyne.Locale{"ru", "ru-RU", "ru_RU"} {
		if got := translationsForLocale(locale); string(got) != string(russianTranslations) {
			t.Errorf("Russian locale %q did not select the Russian catalog", locale)
		}
	}
	if got := translationsForLocale(fyne.Locale("de_DE")); string(got) != string(englishTranslations) {
		t.Error("Unsupported locale did not fall back to English")
	}
}
