/*Copyright [2019] housepower

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package parser

import (
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/forever765/clickhouse_sinker_nali/model"
	"github.com/forever765/clickhouse_sinker_nali/util"
	"github.com/pkg/errors"
	"github.com/valyala/fastjson"
	"go.uber.org/zap"
)

var _ Parser = (*FastjsonParser)(nil)

// FastjsonParser, parser for get data in json format
type FastjsonParser struct {
	pp  *Pool
	fjp fastjson.Parser
}

func (p *FastjsonParser) Parse(bs []byte) (metric model.Metric, err error) {
	var value *fastjson.Value
	if value, err = p.fjp.ParseBytes(bs); err != nil {
		err = errors.Wrapf(err, "")
		return
	}
	metric = &FastjsonMetric{pp: p.pp, value: value}
	return
}

type FastjsonMetric struct {
	pp    *Pool
	value *fastjson.Value
}

func (c *FastjsonMetric) GetString(key string, nullable bool) (val interface{}) {
	v := c.value.Get(key)
	if v == nil || v.Type() == fastjson.TypeNull {
		if nullable {
			return
		}
		val = ""
		return
	}
	switch v.Type() {
	case fastjson.TypeString:
		b, _ := v.StringBytes()
		val = string(b)
	default:
		val = v.String()
	}
	return
}

func (c *FastjsonMetric) GetFloat(key string, nullable bool) (val interface{}) {
	v := c.value.Get(key)
	if !fjCompatibleFloat(v) {
		val = getDefaultFloat(nullable)
		return
	}
	if val2, err := v.Float64(); err != nil {
		val = getDefaultFloat(nullable)
	} else {
		val = val2
	}
	return
}

func (c *FastjsonMetric) GetInt(key string, nullable bool) (val interface{}) {
	v := c.value.Get(key)
	if !fjCompatibleInt(v) {
		val = getDefaultInt(nullable)
		return
	}
	switch v.Type() {
	case fastjson.TypeTrue:
		val = int64(1)
	case fastjson.TypeFalse:
		val = int64(0)
	default:
		if val2, err := v.Int64(); err != nil {
			val = getDefaultInt(nullable)
		} else {
			val = val2
		}
	}
	return
}

func (c *FastjsonMetric) GetDateTime(key string, nullable bool) (val interface{}) {
	v := c.value.Get(key)
	if !fjCompatibleDateTime(v) {
		val = getDefaultDateTime(nullable)
		return
	}
	var err error
	switch v.Type() {
	case fastjson.TypeNumber:
		var f float64
		if f, err = v.Float64(); err != nil {
			val = getDefaultDateTime(nullable)
			return
		}
		val = UnixFloat(f, c.pp.timeUnit)
	case fastjson.TypeString:
		var b []byte
		if b, err = v.StringBytes(); err != nil || len(b) == 0 {
			val = getDefaultDateTime(nullable)
			return
		}
		if val, err = c.pp.ParseDateTime(key, string(b)); err != nil {
			val = getDefaultDateTime(nullable)
		}
	default:
		val = getDefaultDateTime(nullable)
	}
	return
}

func (c *FastjsonMetric) GetElasticDateTime(key string, nullable bool) (val interface{}) {
	t := c.GetDateTime(key, nullable)
	if t != nil {
		val = t.(time.Time).Unix()
	}
	return
}

func (c *FastjsonMetric) GetArray(key string, typ int) (val interface{}) {
	v := c.value.Get(key)
	val = makeArray(typ)
	if v == nil || v.Type() != fastjson.TypeArray {
		return
	}
	array, _ := v.Array()
	switch typ {
	case model.Int:
		for _, e := range array {
			var v int64
			if e.Type() == fastjson.TypeTrue {
				v = 1
			} else {
				v, _ = e.Int64()
			}
			val = append(val.([]int64), v)
		}
	case model.Float:
		for _, e := range array {
			v, _ := e.Float64()
			val = append(val.([]float64), v)
		}
	case model.String:
		for _, e := range array {
			var s string
			switch e.Type() {
			case fastjson.TypeNull:
				s = ""
			case fastjson.TypeString:
				b, _ := e.StringBytes()
				s = string(b)
			default:
				s = e.String()
			}
			val = append(val.([]string), s)
		}
	case model.DateTime:
		for _, e := range array {
			var t time.Time
			switch e.Type() {
			case fastjson.TypeNumber:
				if f, err := e.Float64(); err != nil {
					t = Epoch
				} else {
					t = UnixFloat(f, c.pp.timeUnit)
				}
			case fastjson.TypeString:
				if b, err := e.StringBytes(); err != nil || len(b) == 0 {
					t = Epoch
				} else {
					var err error
					if t, err = c.pp.ParseDateTime(key, string(b)); err != nil {
						t = Epoch
					}
				}
			default:
				t = Epoch
			}
			val = append(val.([]time.Time), t)
		}
	default:
		util.Logger.Fatal(fmt.Sprintf("LOGIC ERROR: unsupported array type %v", typ))
	}
	return
}

func (c *FastjsonMetric) GetNewKeys(knownKeys, newKeys *sync.Map, white, black *regexp.Regexp) (foundNew bool) {
	var obj *fastjson.Object
	var err error
	if obj, err = c.value.Object(); err != nil {
		return
	}
	obj.Visit(func(key []byte, v *fastjson.Value) {
		strKey := string(key)
		if _, loaded := knownKeys.LoadOrStore(strKey, nil); !loaded {
			if (white == nil || white.MatchString(strKey)) &&
				(black == nil || !black.MatchString(strKey)) {
				if typ := fjDetectType(v); typ != model.Unknown {
					newKeys.Store(strKey, typ)
					foundNew = true
				} else {
					util.Logger.Warn("FastjsonMetric.GetNewKeys failed to detect field type", zap.String("key", strKey), zap.String("value", v.String()))
				}
			} else {
				util.Logger.Warn("FastjsonMetric.GetNewKeys ignored new key due to white/black list setting", zap.String("key", strKey), zap.String("value", v.String()))
				knownKeys.Store(strKey, nil)
			}
		}
	})
	return
}

func fjCompatibleInt(v *fastjson.Value) (ok bool) {
	if v == nil {
		return
	}
	switch v.Type() {
	case fastjson.TypeTrue:
		ok = true
	case fastjson.TypeFalse:
		ok = true
	case fastjson.TypeNumber:
		ok = true
	}
	return
}

func fjCompatibleFloat(v *fastjson.Value) (ok bool) {
	if v == nil {
		return
	}
	switch v.Type() {
	case fastjson.TypeNumber:
		ok = true
	}
	return
}

func fjCompatibleDateTime(v *fastjson.Value) (ok bool) {
	if v == nil {
		return
	}
	switch v.Type() {
	case fastjson.TypeNumber:
		ok = true
	case fastjson.TypeString:
		ok = true
	}
	return
}

func getDefaultInt(nullable bool) (val interface{}) {
	if nullable {
		return
	}
	val = int64(0)
	return
}

func getDefaultFloat(nullable bool) (val interface{}) {
	if nullable {
		return
	}
	val = float64(0.0)
	return
}

func getDefaultDateTime(nullable bool) (val interface{}) {
	if nullable {
		return
	}
	val = Epoch
	return
}

func fjDetectType(v *fastjson.Value) (typ int) {
	switch v.Type() {
	case fastjson.TypeNull:
		typ = model.Unknown
	case fastjson.TypeTrue:
		typ = model.Int
	case fastjson.TypeFalse:
		typ = model.Int
	case fastjson.TypeNumber:
		typ = model.Float
		if _, err := v.Int64(); err == nil {
			typ = model.Int
		}
	case fastjson.TypeString:
		typ = model.String
		if val, err := v.StringBytes(); err == nil {
			if _, layout := parseInLocation(string(val), time.Local); layout != "" {
				typ = model.DateTime
			}
		}
	case fastjson.TypeArray:
		if arr, err := v.Array(); err == nil && len(arr) > 0 {
			typ2 := fjDetectType(arr[0])
			switch typ2 {
			case model.Int:
				typ = model.IntArray
			case model.Float:
				typ = model.FloatArray
			case model.String:
				typ = model.StringArray
			case model.DateTime:
				typ = model.DateTimeArray
			}
		}
	default:
		typ = model.String
	}
	return
}
