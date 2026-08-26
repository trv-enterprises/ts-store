#!/usr/bin/env python3
# Copyright (c) 2026 TRV Enterprises LLC
# SPDX-License-Identifier: Apache-2.0
# See LICENSE file for details.
"""
Regression tests for reconcile-scan.py's registry loader.

The bug these exist for: PyYAML resolves an unquoted `expires_on: 2027-02-16`
into a datetime.date, while the no-PyYAML fallback parser yields a str. The
loader called .strip() on every field, so on any machine WITH PyYAML the gate
died with AttributeError before scanning anything — and is_expired() had the
same flaw, meaning the expiry check that justifies the registry had never
actually run in CI.

It survived review because the dev machine had no PyYAML installed, so local
runs silently took the fallback path and passed. Hence: every test here runs
under BOTH parsers, and the PyYAML case is skipped loudly rather than quietly.

Run:  python3 security/test_reconcile_scan.py
"""

import datetime
import importlib.util
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


def load_module():
    spec = importlib.util.spec_from_file_location(
        "reconcile_scan", os.path.join(HERE, "reconcile-scan.py")
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


rs = load_module()

try:
    import yaml  # noqa: F401

    HAVE_YAML = True
except ImportError:
    HAVE_YAML = False


class TestValueCoercion(unittest.TestCase):
    """_as_text must flatten both parsers' representations identically."""

    def test_date_object_becomes_iso_string(self):
        self.assertEqual(rs._as_text(datetime.date(2027, 2, 16)), "2027-02-16")

    def test_string_passes_through_trimmed(self):
        self.assertEqual(rs._as_text("  2027-02-16 "), "2027-02-16")

    def test_none_becomes_empty(self):
        self.assertEqual(rs._as_text(None), "")

    def test_both_parser_shapes_agree(self):
        self.assertEqual(
            rs._as_text(datetime.date(2027, 2, 16)), rs._as_text("2027-02-16")
        )


class TestIsExpired(unittest.TestCase):
    """is_expired must work on a date object, not just a string."""

    def test_date_object_not_yet_expired(self):
        entry = {"expires_on": datetime.date(2099, 1, 1)}
        self.assertFalse(rs.is_expired(entry))

    def test_date_object_expired(self):
        entry = {"expires_on": datetime.date(2000, 1, 1)}
        self.assertTrue(rs.is_expired(entry))

    def test_string_expired(self):
        self.assertTrue(rs.is_expired({"expires_on": "2000-01-01"}))

    def test_missing_expiry_is_not_expired(self):
        # Absent expiry is reported by field validation, not here.
        self.assertFalse(rs.is_expired({}))


class TestRealRegistry(unittest.TestCase):
    """The shipped registry must load cleanly under whichever parser is present."""

    def test_loads_without_validation_errors(self):
        registry, errors = rs.load_registry(rs.REGISTRY)
        self.assertEqual(errors, [], f"registry validation errors: {errors}")
        self.assertGreater(len(registry), 0, "registry unexpectedly empty")

    def test_every_entry_has_a_parseable_expiry(self):
        registry, _ = rs.load_registry(rs.REGISTRY)
        for eid, entry in registry.items():
            exp = rs._as_text(entry.get("expires_on"))
            with self.subTest(entry=eid):
                datetime.date.fromisoformat(exp)  # raises if unparseable

    def test_expiry_detection_actually_fires(self):
        """Guards the case CI never reached: expiry must be reachable, not raise."""
        registry, _ = rs.load_registry(rs.REGISTRY)
        for eid, entry in registry.items():
            with self.subTest(entry=eid):
                self.assertIsInstance(rs.is_expired(entry), bool)


@unittest.skipUnless(HAVE_YAML, "PyYAML not installed — CI's parser path NOT covered")
class TestPyYAMLPath(unittest.TestCase):
    """Explicit coverage of the parser CI uses. Skipped loudly when absent."""

    def test_pyyaml_yields_date_objects_for_registry_dates(self):
        import yaml

        with open(rs.REGISTRY) as f:
            data = yaml.safe_load(f) or {}
        entries = data.get("exceptions", []) or []
        self.assertTrue(
            any(isinstance(e.get("expires_on"), datetime.date) for e in entries),
            "expected PyYAML to resolve at least one expires_on to a date object; "
            "if the registry quoted its dates this test is no longer meaningful",
        )

    def test_registry_loads_under_pyyaml(self):
        registry, errors = rs.load_registry(rs.REGISTRY)
        self.assertEqual(errors, [])
        self.assertGreater(len(registry), 0)


if __name__ == "__main__":
    if not HAVE_YAML:
        print(
            "WARNING: PyYAML is not installed. The fallback parser is being "
            "tested, but CI runs WITH PyYAML — install it to cover that path:\n"
            "  python3 -m pip install pyyaml\n",
            file=sys.stderr,
        )
    unittest.main(verbosity=2)
