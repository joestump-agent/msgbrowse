//go:build darwin && macoscontacts && cgo

// The real macOS Contacts.framework binding — the ONLY cgo/Objective-C in this
// package, compiled solely under `darwin && macoscontacts && cgo`. It is
// deliberately thin: it exposes exactly two operations to the pure-Go Provider —
// read the current authorization status, and dump every contact's raw
// identifier / name / phones / emails into the serialized form parseDump
// consumes. NO classification, normalization, or matching happens here; that all
// lives in provider.go and is unit-tested without cgo, so the only logic that is
// not compile-verified in this project's CGO_ENABLED=0 CI is this framework glue.
//
// Detect-only: authorization() reads authorizationStatusForEntityType and NEVER
// calls requestAccess, so it cannot trigger the interactive TCC prompt — the
// merge settings UI (#12) drives the grant flow explicitly, mirroring the
// detect-and-guide model of internal/setup (ADR-0020).
package macoscontacts

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Contacts -framework Foundation

#import <Contacts/Contacts.h>
#include <stdlib.h>
#include <string.h>

// mb_contacts_auth_status returns the raw CNAuthorizationStatus for the
// contacts entity type. It never prompts.
static int mb_contacts_auth_status(void) {
	return (int)[CNContactStore authorizationStatusForEntityType:CNEntityTypeContacts];
}

// mb_contacts_dump enumerates every contact and returns a newly-allocated C
// string (caller frees) in the record/field format parseDump reads:
//   record  := key <US> displayName ( <US> token )*
//   token   := 'p' phoneString | 'e' emailString
//   records separated by <RS> (0x1e); fields by <US> (0x1f).
// On failure it returns NULL and, when errmsg is non-NULL, sets *errmsg to a
// newly-allocated description (caller frees).
static char *mb_contacts_dump(char **errmsg) {
	@autoreleasepool {
		CNContactStore *store = [[CNContactStore alloc] init];
		id<CNKeyDescriptor> nameKeys =
			[CNContactFormatter descriptorForRequiredKeysForStyle:CNContactFormatterStyleFullName];
		NSArray *keys = @[
			CNContactIdentifierKey,
			CNContactOrganizationNameKey,
			CNContactPhoneNumbersKey,
			CNContactEmailAddressesKey,
			nameKeys,
		];
		CNContactFetchRequest *req = [[CNContactFetchRequest alloc] initWithKeysToFetch:keys];
		NSMutableString *out = [NSMutableString string];
		__block BOOL first = YES;
		NSError *err = nil;
		BOOL ok = [store enumerateContactsWithFetchRequest:req
													 error:&err
												usingBlock:^(CNContact *c, BOOL *stop) {
			(void)stop;
			if (!first) {
				[out appendString:@"\x1e"];
			}
			first = NO;
			[out appendString:(c.identifier ?: @"")];
			[out appendString:@"\x1f"];
			NSString *name = [CNContactFormatter stringFromContact:c style:CNContactFormatterStyleFullName];
			if (name == nil || name.length == 0) {
				name = (c.organizationName ?: @"");
			}
			[out appendString:name];
			for (CNLabeledValue<CNPhoneNumber *> *ph in c.phoneNumbers) {
				NSString *v = ph.value.stringValue;
				if (v == nil) {
					continue;
				}
				[out appendString:@"\x1f"];
				[out appendString:@"p"];
				[out appendString:v];
			}
			for (CNLabeledValue<NSString *> *em in c.emailAddresses) {
				NSString *v = em.value;
				if (v == nil) {
					continue;
				}
				[out appendString:@"\x1f"];
				[out appendString:@"e"];
				[out appendString:v];
			}
		}];
		if (!ok) {
			if (errmsg != NULL) {
				const char *desc = [[err localizedDescription] UTF8String];
				*errmsg = strdup(desc != NULL ? desc : "contacts enumeration failed");
			}
			return NULL;
		}
		const char *utf8 = [out UTF8String];
		return strdup(utf8 != NULL ? utf8 : "");
	}
}
*/
import "C"

import (
	"context"
	"errors"
	"unsafe"
)

// backendCompiledIn reports that this build DID link the real
// Contacts.framework backend (see backend_stub.go for the false case).
const backendCompiledIn = true

// cnBackend is the live Contacts.framework backend.
type cnBackend struct{}

// newBackend returns the real Contacts backend. Construction cannot fail (the
// CNContactStore is created lazily per call), so the error is always nil; the
// signature matches the stub so New is build-tag-agnostic.
func newBackend() (backend, error) { return cnBackend{}, nil }

// authorization reads the framework's CNAuthorizationStatus and maps it through
// the shared pure-Go table. It never prompts.
func (cnBackend) authorization(context.Context) authStatus {
	return authStatusFromCN(int(C.mb_contacts_auth_status()))
}

// people enumerates the address book via the thin C dump and parses the result
// with the pure-Go parseDump. A framework error surfaces as a Go error.
func (cnBackend) people(context.Context) ([]rawPerson, error) {
	var cerr *C.char
	out := C.mb_contacts_dump(&cerr)
	if out == nil {
		msg := "contacts enumeration failed"
		if cerr != nil {
			msg = C.GoString(cerr)
			C.free(unsafe.Pointer(cerr))
		}
		return nil, errors.New(msg)
	}
	defer C.free(unsafe.Pointer(out))
	return parseDump(C.GoString(out)), nil
}
