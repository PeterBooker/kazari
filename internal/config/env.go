package config

import (
	"encoding"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

const envPrefix = "KAZARI"

// applyEnv walks cfg by its yaml tags, so a new key gets its KAZARI_*
// variable without further code.
func applyEnv(cfg *Config, lookup func(string) (string, bool)) error {
	_, err := applyEnvStruct(reflect.ValueOf(cfg).Elem(), envPrefix, lookup)
	return err
}

// applyEnvStruct reports whether any variable was set.
func applyEnvStruct(v reflect.Value, prefix string, lookup func(string) (string, bool)) (bool, error) {
	t := v.Type()
	applied := false
	var errs []error
	for i := range t.NumField() {
		f := t.Field(i)
		name := keyName(f)
		if name == "" {
			continue
		}
		key := prefix + "_" + strings.ToUpper(name)
		fv := v.Field(i)
		switch {
		case f.Type.Kind() == reflect.Pointer && f.Type.Elem().Kind() == reflect.Struct:
			// An optional mapping: fill a defaulted copy, kept only if one of
			// its variables is set.
			target := fv
			if fv.IsNil() {
				target = reflect.New(f.Type.Elem())
				if d, ok := target.Interface().(interface{ applyDefaults() }); ok {
					d.applyDefaults()
				}
			}
			ok, err := applyEnvStruct(target.Elem(), key, lookup)
			if err != nil {
				errs = append(errs, err)
			}
			if ok {
				applied = true
				fv.Set(target)
			}
		case f.Type.Kind() == reflect.Struct && !isTextUnmarshaler(fv):
			ok, err := applyEnvStruct(fv, key, lookup)
			if err != nil {
				errs = append(errs, err)
			}
			applied = applied || ok
		default:
			raw, ok := lookup(key)
			if !ok {
				continue
			}
			if err := setFromString(fv, raw); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", key, err))
				continue
			}
			applied = true
		}
	}
	return applied, errors.Join(errs...)
}

func isTextUnmarshaler(v reflect.Value) bool {
	_, ok := v.Addr().Interface().(encoding.TextUnmarshaler)
	return ok
}

func setFromString(v reflect.Value, raw string) error {
	if u, ok := v.Addr().Interface().(encoding.TextUnmarshaler); ok {
		return u.UnmarshalText([]byte(raw))
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid boolean %q", raw)
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid integer %q", raw)
		}
		v.SetInt(n)
	case reflect.Slice:
		if v.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported list type %s", v.Type())
		}
		list := []string{}
		for item := range strings.SplitSeq(raw, ",") {
			if item = strings.TrimSpace(item); item != "" {
				list = append(list, item)
			}
		}
		v.Set(reflect.ValueOf(list).Convert(v.Type()))
	default:
		return fmt.Errorf("unsupported type %s", v.Type())
	}
	return nil
}
