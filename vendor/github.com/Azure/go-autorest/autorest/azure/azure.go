/*
Package azure provides Azure-specific implementations used with AutoRest.

See the included examples for more detail.
*/
package azure

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"time"

	"github.com/Azure/go-autorest/autorest"
)

const (
	// HeaderAsyncOperation is the Azure header containing the location to poll for long-running
	// operations.
	HeaderAsyncOperation = "Azure-AsyncOperation"

	// HeaderClientID is the Azure extension header to set a user-specified request ID.
	HeaderClientID = "x-ms-client-request-id"

	// HeaderReturnClientID is the Azure extension header to set if the user-specified request ID
	// should be included in the response.
	HeaderReturnClientID = "x-ms-return-client-request-id"

	// HeaderRequestID is the Azure extension header of the service generated request ID returned
	// in the response.
	HeaderRequestID = "x-ms-request-id"
)

// ServiceError encapsulates the error response from an Azure service.
type ServiceError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RequestError describes an error response returned by Azure service.
type RequestError struct {
	autorest.DetailedError

	// The error returned by the Azure service.
	ServiceError *ServiceError `json:"error"`

	// The request id (from the x-ms-request-id-header) of the request.
	RequestID string
}

// Error returns a human-friendly error message from service error.
func (e RequestError) Error() string {
	return fmt.Sprintf("azure: Service returned an error. Code=%q Message=%q Status=%d",
		e.ServiceError.Code, e.ServiceError.Message, e.StatusCode)
}

// IsAzureError returns true if the passed error is an Azure Service error; false otherwise.
func IsAzureError(e error) bool {
	_, ok := e.(*RequestError)
	return ok
}

// NewErrorWithError creates a new Error conforming object from the
// passed packageType, method, statusCode of the given resp (UndefinedStatusCode
// if resp is nil), message, and original error. message is treated as a format
// string to which the optional args apply.
func NewErrorWithError(original error, packageType string, method string, resp *http.Response, message string, args ...interface{}) RequestError {
	if v, ok := original.(*RequestError); ok {
		return *v
	}

	statusCode := autorest.UndefinedStatusCode
	if resp != nil {
		statusCode = resp.StatusCode
	}
	return RequestError{
		DetailedError: autorest.DetailedError{
			Original:    original,
			PackageType: packageType,
			Method:      method,
			StatusCode:  statusCode,
			Message:     fmt.Sprintf(message, args...),
		},
	}
}

// WithReturningClientID returns a PrepareDecorator that adds an HTTP extension header of
// x-ms-client-request-id whose value is the passed, undecorated UUID (e.g.,
// "0F39878C-5F76-4DB8-A25D-61D2C193C3CA"). It also sets the x-ms-return-client-request-id
// header to true such that UUID accompanies the http.Response.
func WithReturningClientID(uuid string) autorest.PrepareDecorator {
	preparer := autorest.CreatePreparer(
		WithClientID(uuid),
		WithReturnClientID(true))

	return func(p autorest.Preparer) autorest.Preparer {
		return autorest.PreparerFunc(func(r *http.Request) (*http.Request, error) {
			r, err := p.Prepare(r)
			if err != nil {
				return r, err
			}
			return preparer.Prepare(r)
		})
	}
}

// WithClientID returns a PrepareDecorator that adds an HTTP extension header of
// x-ms-client-request-id whose value is passed, undecorated UUID (e.g.,
// "0F39878C-5F76-4DB8-A25D-61D2C193C3CA").
func WithClientID(uuid string) autorest.PrepareDecorator {
	return autorest.WithHeader(HeaderClientID, uuid)
}

// WithReturnClientID returns a PrepareDecorator that adds an HTTP extension header of
// x-ms-return-client-request-id whose boolean value indicates if the value of the
// x-ms-client-request-id header should be included in the http.Response.
func WithReturnClientID(b bool) autorest.PrepareDecorator {
	return autorest.WithHeader(HeaderReturnClientID, strconv.FormatBool(b))
}

// ExtractClientID extracts the client identifier from the x-ms-client-request-id header set on the
// http.Request sent to the service (and returned in the http.Response)
func ExtractClientID(resp *http.Response) string {
	return autorest.ExtractHeaderValue(HeaderClientID, resp)
}

// ExtractRequestID extracts the Azure server generated request identifier from the
// x-ms-request-id header.
func ExtractRequestID(resp *http.Response) string {
	return autorest.ExtractHeaderValue(HeaderRequestID, resp)
}

// GetAsyncOperation retrieves the long-running URL to poll from the passed response.
func GetAsyncOperation(resp *http.Response) string {
	return resp.Header.Get(http.CanonicalHeaderKey(HeaderAsyncOperation))
}

// ResponseIsLongRunning returns true if the passed response is for an Azure long-running operation.
func ResponseIsLongRunning(resp *http.Response) bool {
	return autorest.ResponseRequiresPolling(resp, http.StatusCreated) && GetAsyncOperation(resp) != ""
}

// NewAsyncPollingRequest allocates and returns a new http.Request to poll an Azure long-running
// operation. If it successfully creates the request, it will also close the body of the passed
// response, otherwise the body remains open.
func NewAsyncPollingRequest(resp *http.Response, c autorest.Client) (*http.Request, error) {
	location := GetAsyncOperation(resp)
	if location == "" {
		return nil, autorest.NewErrorWithResponse("azure", "NewAsyncPollingRequest", resp, "Azure-AsyncOperation header missing from response that requires polling")
	}

	req, err := autorest.Prepare(&http.Request{},
		autorest.AsGet(),
		autorest.WithBaseURL(location))
	if err != nil {
		return nil, autorest.NewErrorWithError(err, "azure", "NewAsyncPollingRequest", nil, "Failure creating poll request to %s", location)
	}

	autorest.Respond(resp,
		c.ByInspecting(),
		autorest.ByClosing())

	return req, nil
}

// WithAsyncPolling will poll until the completion of an Azure long-running operation. The delay
// time between requests is taken from the HTTP Retry-After header, if present, or the passed
// delay otherwise. Polling may be canceled by signaling on the optional http.Request channel.
func WithAsyncPolling(defaultDelay time.Duration) autorest.SendDecorator {
	return func(s autorest.Sender) autorest.Sender {
		return autorest.SenderFunc(func(r *http.Request) (*http.Response, error) {
			resp, err := s.Do(r)
			for err == nil && ResponseIsLongRunning(resp) {
				err = autorest.DelayForBackoff(autorest.GetPollingDelay(resp, defaultDelay), 1, r.Cancel)
				if err == nil {
					resp, err = s.Do(r)
				}
			}
			return resp, err
		})
	}
}

// WithErrorUnlessStatusCode returns a RespondDecorator that emits an
// azure.RequestError by reading the response body unless the response HTTP status code
// is among the set passed.
//
// If there is a chance service may return responses other than the Azure error
// format and the response cannot be parsed into an error, a decoding error will
// be returned containing the response body. In any case, the Responder will
// return an error if the status code is not satisfied.
//
// If this Responder returns an error, the response body will be replaced with
// an in-memory reader, which needs no further closing.
func WithErrorUnlessStatusCode(codes ...int) autorest.RespondDecorator {
	return func(r autorest.Responder) autorest.Responder {
		return autorest.ResponderFunc(func(resp *http.Response) error {
			err := r.Respond(resp)
			if err == nil && !autorest.ResponseHasStatusCode(resp, codes...) {
				var e RequestError
				defer resp.Body.Close()

				b, decodeErr := autorest.CopyAndDecode(autorest.EncodedAsJSON, resp.Body, &e)
				resp.Body = ioutil.NopCloser(&b) // replace body with in-memory reader
				if decodeErr != nil || e.ServiceError == nil {
					return fmt.Errorf("autorest/azure: error response cannot be parsed: %q error: %v", b.String(), err)
				}

				e.RequestID = ExtractRequestID(resp)
				e.StatusCode = resp.StatusCode
				err = &e
			}
			return err
		})
	}
}
