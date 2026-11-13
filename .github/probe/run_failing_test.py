"""Throwaway probe: run main's failing seed TLS test, printing what wait_peer swallows."""
import sys
import unittest

sys.path.insert(0, 'internal/seedtls/testdata')
import test_tls

original = test_tls.renew.peer


def peer(*args, **kwargs):
    try:
        return original(*args, **kwargs)
    except Exception as e:
        print('peer%r raised %s: %s (verify_code=%s, verify_message=%r)' % (
            args[1:], type(e).__name__, e, getattr(e, 'verify_code', None), getattr(e, 'verify_message', None)), flush=True)
        raise


test_tls.renew.peer = peer
result = unittest.main(module=test_tls, argv=['probe', '-v', 'CertificateTests.test_real_served_tls_validates_both_identities'], exit=False).result
print('test passed:', result.wasSuccessful())
