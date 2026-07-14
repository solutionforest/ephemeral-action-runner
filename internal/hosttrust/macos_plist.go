package hosttrust

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type macPlistKind uint8

const (
	macPlistDictionary macPlistKind = iota + 1
	macPlistArray
	macPlistString
	macPlistInteger
	macPlistData
	macPlistOther
)

type macPlistValue struct {
	kind       macPlistKind
	dictionary map[string]macPlistValue
	array      []macPlistValue
	text       string
	integer    int64
}

func parseMacXMLPlist(content []byte) (macPlistValue, error) {
	decoder := xml.NewDecoder(strings.NewReader(string(content)))
	rootToken, err := nextMacPlistToken(decoder)
	if err != nil {
		return macPlistValue{}, fmt.Errorf("read plist root: %w", err)
	}
	root, ok := rootToken.(xml.StartElement)
	if !ok || root.Name.Local != "plist" || root.Name.Space != "" {
		return macPlistValue{}, fmt.Errorf("plist root element is required")
	}
	if len(root.Attr) != 1 || root.Attr[0].Name.Local != "version" || root.Attr[0].Name.Space != "" || root.Attr[0].Value != "1.0" {
		return macPlistValue{}, fmt.Errorf("plist version must be 1.0")
	}
	valueToken, err := nextMacPlistToken(decoder)
	if err != nil {
		return macPlistValue{}, fmt.Errorf("read plist value: %w", err)
	}
	valueStart, ok := valueToken.(xml.StartElement)
	if !ok {
		return macPlistValue{}, fmt.Errorf("plist must contain one value")
	}
	value, err := decodeMacPlistValue(decoder, valueStart)
	if err != nil {
		return macPlistValue{}, err
	}
	endToken, err := nextMacPlistToken(decoder)
	if err != nil {
		return macPlistValue{}, fmt.Errorf("read plist end: %w", err)
	}
	end, ok := endToken.(xml.EndElement)
	if !ok || end.Name != root.Name {
		return macPlistValue{}, fmt.Errorf("plist must contain exactly one value")
	}
	if token, err := nextMacPlistToken(decoder); err != io.EOF {
		if err != nil {
			return macPlistValue{}, fmt.Errorf("read after plist: %w", err)
		}
		return macPlistValue{}, fmt.Errorf("unexpected content after plist: %T", token)
	}
	return value, nil
}

func nextMacPlistToken(decoder *xml.Decoder) (xml.Token, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(value)) == "" {
				continue
			}
		case xml.Comment, xml.Directive, xml.ProcInst:
			continue
		}
		return token, nil
	}
}

func decodeMacPlistValue(decoder *xml.Decoder, start xml.StartElement) (macPlistValue, error) {
	if start.Name.Space != "" || len(start.Attr) != 0 {
		return macPlistValue{}, fmt.Errorf("unsupported plist element %s attributes or namespace", start.Name.Local)
	}
	switch start.Name.Local {
	case "dict":
		return decodeMacPlistDictionary(decoder, start)
	case "array":
		return decodeMacPlistArray(decoder, start)
	case "string":
		text, err := decodeMacPlistText(decoder, start)
		return macPlistValue{kind: macPlistString, text: text}, err
	case "integer":
		text, err := decodeMacPlistText(decoder, start)
		if err != nil {
			return macPlistValue{}, err
		}
		integer, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return macPlistValue{}, fmt.Errorf("invalid plist integer %q", text)
		}
		return macPlistValue{kind: macPlistInteger, integer: integer}, nil
	case "data":
		text, err := decodeMacPlistText(decoder, start)
		if err != nil {
			return macPlistValue{}, err
		}
		encoded := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, text)
		if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
			return macPlistValue{}, fmt.Errorf("invalid plist data: %w", err)
		}
		return macPlistValue{kind: macPlistData}, nil
	case "date":
		text, err := decodeMacPlistText(decoder, start)
		if err != nil {
			return macPlistValue{}, err
		}
		if _, err := time.Parse(time.RFC3339, strings.TrimSpace(text)); err != nil {
			return macPlistValue{}, fmt.Errorf("invalid plist date %q", text)
		}
		return macPlistValue{kind: macPlistOther}, nil
	case "real":
		text, err := decodeMacPlistText(decoder, start)
		if err != nil {
			return macPlistValue{}, err
		}
		if _, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err != nil {
			return macPlistValue{}, fmt.Errorf("invalid plist real %q", text)
		}
		return macPlistValue{kind: macPlistOther}, nil
	case "true", "false":
		text, err := decodeMacPlistText(decoder, start)
		if err != nil {
			return macPlistValue{}, err
		}
		if strings.TrimSpace(text) != "" {
			return macPlistValue{}, fmt.Errorf("plist %s must be empty", start.Name.Local)
		}
		return macPlistValue{kind: macPlistOther}, nil
	default:
		return macPlistValue{}, fmt.Errorf("unsupported plist value element %q", start.Name.Local)
	}
}

func decodeMacPlistDictionary(decoder *xml.Decoder, start xml.StartElement) (macPlistValue, error) {
	dictionary := make(map[string]macPlistValue)
	for {
		token, err := nextMacPlistToken(decoder)
		if err != nil {
			return macPlistValue{}, fmt.Errorf("read plist dictionary: %w", err)
		}
		if end, ok := token.(xml.EndElement); ok {
			if end.Name != start.Name {
				return macPlistValue{}, fmt.Errorf("unexpected plist end element %q", end.Name.Local)
			}
			return macPlistValue{kind: macPlistDictionary, dictionary: dictionary}, nil
		}
		keyStart, ok := token.(xml.StartElement)
		if !ok || keyStart.Name.Local != "key" || keyStart.Name.Space != "" || len(keyStart.Attr) != 0 {
			return macPlistValue{}, fmt.Errorf("plist dictionary key element is required")
		}
		key, err := decodeMacPlistText(decoder, keyStart)
		if err != nil {
			return macPlistValue{}, err
		}
		if _, duplicate := dictionary[key]; duplicate {
			return macPlistValue{}, fmt.Errorf("duplicate plist dictionary key %q", key)
		}
		valueToken, err := nextMacPlistToken(decoder)
		if err != nil {
			return macPlistValue{}, fmt.Errorf("read plist dictionary value for %q: %w", key, err)
		}
		valueStart, ok := valueToken.(xml.StartElement)
		if !ok {
			return macPlistValue{}, fmt.Errorf("plist dictionary value for %q is required", key)
		}
		value, err := decodeMacPlistValue(decoder, valueStart)
		if err != nil {
			return macPlistValue{}, fmt.Errorf("plist dictionary value for %q: %w", key, err)
		}
		dictionary[key] = value
	}
}

func decodeMacPlistArray(decoder *xml.Decoder, start xml.StartElement) (macPlistValue, error) {
	var array []macPlistValue
	for {
		token, err := nextMacPlistToken(decoder)
		if err != nil {
			return macPlistValue{}, fmt.Errorf("read plist array: %w", err)
		}
		if end, ok := token.(xml.EndElement); ok {
			if end.Name != start.Name {
				return macPlistValue{}, fmt.Errorf("unexpected plist end element %q", end.Name.Local)
			}
			return macPlistValue{kind: macPlistArray, array: array}, nil
		}
		valueStart, ok := token.(xml.StartElement)
		if !ok {
			return macPlistValue{}, fmt.Errorf("plist array value is required")
		}
		value, err := decodeMacPlistValue(decoder, valueStart)
		if err != nil {
			return macPlistValue{}, err
		}
		array = append(array, value)
	}
}

func decodeMacPlistText(decoder *xml.Decoder, start xml.StartElement) (string, error) {
	var text strings.Builder
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", fmt.Errorf("read plist %s: %w", start.Name.Local, err)
		}
		switch value := token.(type) {
		case xml.CharData:
			text.Write(value)
		case xml.Comment:
			continue
		case xml.EndElement:
			if value.Name != start.Name {
				return "", fmt.Errorf("unexpected plist end element %q", value.Name.Local)
			}
			return text.String(), nil
		default:
			return "", fmt.Errorf("plist %s must contain text only", start.Name.Local)
		}
	}
}
