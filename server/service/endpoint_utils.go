package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/fleetdm/fleet/v4/server/fleet"
	kithttp "github.com/go-kit/kit/transport/http"
	"github.com/gorilla/mux"
)

type handlerFunc func(ctx context.Context, request interface{}, svc fleet.Service) (interface{}, error)

// parseTag parses a `url` tag and whether it's optional or not, which is an optional part of the tag
func parseTag(tag string) (string, bool, error) {
	parts := strings.Split(tag, ",")
	switch len(parts) {
	case 0:
		return "", false, fmt.Errorf("Error parsing %s: too few parts", tag)
	case 1:
		return tag, false, nil
	case 2:
		return parts[0], parts[1] == "optional", nil
	default:
		return "", false, fmt.Errorf("Error parsing %s: too many parts", tag)
	}
}

// allFields returns all the fields for a struct, including the ones from embedded structs
func allFields(ifv reflect.Value) []reflect.StructField {
	if ifv.Kind() == reflect.Ptr {
		ifv = ifv.Elem()
	}
	if ifv.Kind() != reflect.Struct {
		return nil
	}

	var fields []reflect.StructField

	if !ifv.IsValid() {
		return nil
	}

	t := ifv.Type()

	for i := 0; i < ifv.NumField(); i++ {
		v := ifv.Field(i)

		if v.Kind() == reflect.Struct && t.Field(i).Anonymous {
			fields = append(fields, allFields(v)...)
			continue
		}
		fields = append(fields, ifv.Type().Field(i))
	}

	return fields
}

// makeDecoder creates a decoder for the type for the struct passed on. If the
// struct has at least 1 json tag it'll unmarshall the body. If the struct has
// a `url` tag with value list_options it'll gather fleet.ListOptions from the
// URL (similarly for host_options, carve_options that derive from the common
// list_options).
//
// Finally, any other `url` tag will be treated as a path variable (of the form
// /path/{name} in the route's path) from the URL path pattern, and it'll be
// decoded and set accordingly. Variables can be optional by setting the tag as
// follows: `url:"some-id,optional"`.
// The "list_options" are optional by default and it'll ignore the optional
// portion of the tag.
func makeDecoder(iface interface{}) kithttp.DecodeRequestFunc {
	if iface == nil {
		return func(ctx context.Context, r *http.Request) (interface{}, error) {
			return nil, nil
		}
	}
	t := reflect.TypeOf(iface)
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("makeDecoder only understands structs, not %T", iface))
	}

	return func(ctx context.Context, r *http.Request) (interface{}, error) {
		v := reflect.New(t)
		nilBody := false

		buf := bufio.NewReader(r.Body)
		if _, err := buf.Peek(1); err == io.EOF {
			nilBody = true
		} else {
			req := v.Interface()
			if err := json.NewDecoder(buf).Decode(req); err != nil {
				return nil, err
			}
			v = reflect.ValueOf(req)
		}

		for _, f := range allFields(v) {
			field := v.Elem().FieldByName(f.Name)

			urlTagValue, ok := f.Tag.Lookup("url")

			optional := false
			var err error
			if ok {
				urlTagValue, optional, err = parseTag(urlTagValue)
				if err != nil {
					return nil, err
				}

				switch urlTagValue {
				case "list_options":
					opts, err := listOptionsFromRequest(r)
					if err != nil {
						return nil, err
					}
					field.Set(reflect.ValueOf(opts))

				case "host_options":
					opts, err := hostListOptionsFromRequest(r)
					if err != nil {
						return nil, err
					}
					field.Set(reflect.ValueOf(opts))

				case "carve_options":
					opts, err := carveListOptionsFromRequest(r)
					if err != nil {
						return nil, err
					}
					field.Set(reflect.ValueOf(opts))

				default:
					switch field.Kind() {
					case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
						v, err := intFromRequest(r, urlTagValue)
						if err != nil {
							if err == errBadRoute && optional {
								continue
							}
							return nil, err
						}
						field.SetInt(v)

					case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
						v, err := uintFromRequest(r, urlTagValue)
						if err != nil {
							if err == errBadRoute && optional {
								continue
							}
							return nil, err
						}
						field.SetUint(v)

					case reflect.String:
						v, err := stringFromRequest(r, urlTagValue)
						if err != nil {
							if err == errBadRoute && optional {
								continue
							}
							return nil, err
						}
						field.SetString(v)

					default:
						return nil, fmt.Errorf("unsupported type for field %s for 'url' decoding: %s", urlTagValue, field.Kind())
					}
				}
			}

			_, jsonExpected := f.Tag.Lookup("json")
			if jsonExpected && nilBody {
				return nil, errors.New("Expected JSON Body")
			}

			queryTagValue, ok := f.Tag.Lookup("query")

			if ok {
				queryTagValue, optional, err = parseTag(queryTagValue)
				if err != nil {
					return nil, err
				}
				queryVal := r.URL.Query().Get(queryTagValue)
				// if optional and it's a ptr, leave as nil
				if queryVal == "" {
					if optional {
						continue
					}
					return nil, fmt.Errorf("Param %s is required", f.Name)
				}
				if field.Kind() == reflect.Ptr {
					// create the new instance of whatever it is
					field.Set(reflect.New(field.Type().Elem()))
					field = field.Elem()
				}
				switch field.Kind() {
				case reflect.String:
					field.SetString(queryVal)
				case reflect.Uint:
					queryValUint, err := strconv.Atoi(queryVal)
					if err != nil {
						return nil, fmt.Errorf("parsing uint from query: %w", err)
					}
					field.SetUint(uint64(queryValUint))
				case reflect.Bool:
					field.SetBool(queryVal == "1" || queryVal == "true")
				case reflect.Int:
					queryValInt := 0
					switch queryTagValue {
					case "order_direction":
						switch queryVal {
						case "desc":
							queryValInt = int(fleet.OrderDescending)
						case "asc":
							queryValInt = int(fleet.OrderAscending)
						case "":
							queryValInt = int(fleet.OrderAscending)
						default:
							return fleet.ListOptions{},
								errors.New("unknown order_direction: " + queryVal)
						}
					default:
						queryValInt, err = strconv.Atoi(queryVal)
						if err != nil {
							return nil, fmt.Errorf("parsing uint from query: %w", err)
						}
					}
					field.SetInt(int64(queryValInt))
				default:
					return nil, fmt.Errorf("Cant handle type for field %s %s", f.Name, field.Kind())
				}
			}
		}

		return v.Interface(), nil
	}
}

type UserAuthEndpointer struct {
	svc               fleet.Service
	opts              []kithttp.ServerOption
	r                 *mux.Router
	versions          []string
	startingAtVersion string
	endingAtVersion   string
	alternativePaths  []string
}

func NewUserAuthenticatedEndpointer(svc fleet.Service, opts []kithttp.ServerOption, r *mux.Router, versions ...string) *UserAuthEndpointer {
	return &UserAuthEndpointer{svc: svc, opts: opts, r: r, versions: versions}
}

func getNameFromPathAndVerb(verb, path string) string {
	return strings.ToLower(verb) + "_" + strings.ReplaceAll(path, "/", "_")
}

func (e *UserAuthEndpointer) POST(path string, f handlerFunc, v interface{}) {
	e.handle(path, f, v, "POST")
}

func (e *UserAuthEndpointer) GET(path string, f handlerFunc, v interface{}) {
	e.handle(path, f, v, "GET")
}

func (e *UserAuthEndpointer) PATCH(path string, f handlerFunc, v interface{}) {
	e.handle(path, f, v, "PATCH")
}

func (e *UserAuthEndpointer) DELETE(path string, f handlerFunc, v interface{}) {
	e.handle(path, f, v, "DELETE")
}

func (e *UserAuthEndpointer) handle(path string, f handlerFunc, v interface{}, verb string) {
	versions := e.versions
	if e.startingAtVersion != "" {
		startIndex := -1
		for i, version := range versions {
			if version == e.startingAtVersion {
				startIndex = i
				break
			}
		}
		if startIndex == -1 {
			panic("StartAtVersion is not part of the valid versions")
		}
		versions = versions[startIndex:]
	}
	if e.endingAtVersion != "" {
		endIndex := -1
		for i, version := range versions {
			if version == e.endingAtVersion {
				endIndex = i
				break
			}
		}
		if endIndex == -1 {
			panic("EndAtVersion is not part of the valid versions")
		}
		versions = versions[:endIndex+1]
	}

	// if a version doesn't have a deprecation version, or the ending version is the latest one, then it's part of the
	// latest
	if e.endingAtVersion == "" || e.endingAtVersion == e.versions[len(e.versions)-1] {
		versions = append(versions, "latest")
	}

	versionedPath := strings.Replace(path, "/_version_/", fmt.Sprintf("/{fleetversion:(?:%s)}/", strings.Join(versions, "|")), 1)
	nameAndVerb := getNameFromPathAndVerb(verb, path)
	endpoint := e.makeEndpoint(f, v)
	e.r.Handle(versionedPath, endpoint).Name(nameAndVerb).Methods(verb)
	for _, alias := range e.alternativePaths {
		versionedPath := strings.Replace(alias, "/_version_/", fmt.Sprintf("/{fleetversion:(?:%s)}/", strings.Join(versions, "|")), 1)
		e.r.Handle(versionedPath, endpoint).Name(nameAndVerb).Methods(verb)
	}
}

func (e *UserAuthEndpointer) makeEndpoint(f handlerFunc, v interface{}) http.Handler {
	return newServer(
		authenticatedUser(
			e.svc,
			func(ctx context.Context, request interface{}) (interface{}, error) {
				return f(ctx, request, e.svc)
			}),
		makeDecoder(v),
		e.opts,
	)
}

func (e *UserAuthEndpointer) StartingAtVersion(version string) *UserAuthEndpointer {
	return &UserAuthEndpointer{
		svc:               e.svc,
		opts:              e.opts,
		r:                 e.r,
		versions:          e.versions,
		startingAtVersion: version,
		endingAtVersion:   e.endingAtVersion,
		alternativePaths:  e.alternativePaths,
	}
}

func (e *UserAuthEndpointer) EndingAtVersion(version string) *UserAuthEndpointer {
	return &UserAuthEndpointer{
		svc:               e.svc,
		opts:              e.opts,
		r:                 e.r,
		versions:          e.versions,
		startingAtVersion: e.startingAtVersion,
		endingAtVersion:   version,
		alternativePaths:  e.alternativePaths,
	}
}

func (e *UserAuthEndpointer) WithAltPaths(paths ...string) *UserAuthEndpointer {
	return &UserAuthEndpointer{
		svc:               e.svc,
		opts:              e.opts,
		r:                 e.r,
		versions:          e.versions,
		startingAtVersion: e.startingAtVersion,
		endingAtVersion:   e.endingAtVersion,
		alternativePaths:  paths,
	}
}
