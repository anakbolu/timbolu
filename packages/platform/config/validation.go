package config

import (
	"errors"
	"reflect"
)

// Validate checks that every required config field is populated. Fields tagged
// `validate:"optional"` are skipped.
func (c *Config) Validate() error {
	if err := validateStruct(c.Http, "http"); err != nil {
		return err
	}
	if err := validateStruct(c.Database, "database"); err != nil {
		return err
	}
	if err := validateStruct(c.JWT, "jwt"); err != nil {
		return err
	}
	return nil
}

func validateStruct(cfg interface{}, name string) error {
	v := reflect.ValueOf(cfg)
	t := v.Type()
	if v.Kind() != reflect.Struct {
		return errors.New("provided value is not a struct")
	}

	for i := range v.NumField() {
		field := v.Field(i)
		fieldType := t.Field(i)

		if field.Kind() == reflect.Bool {
			continue
		}
		if fieldType.Tag.Get("validate") == "optional" {
			continue
		}
		if field.IsZero() {
			return errors.New("err " + name + " config: field " + fieldType.Name + " is required")
		}
	}

	return nil
}
