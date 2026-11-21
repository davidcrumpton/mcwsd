"""The coverage gate.

``case_inventory.CASES`` is the inventory of record and every ported test
module declares which case each of its test methods covers. This module
cross-checks the two, and fails when:

* a case in the inventory has no test,
* a mapped test method no longer exists,
* a mapped case id is not in the inventory,
* the inventory contains a duplicate case id,
* a test method exists that is mapped to no case (an unrecorded behaviour),
* a case names a section that is not declared, or a kind that is not
  positive/negative.

Two consequences worth remembering:

1. A case name is a promise, not a label. Renaming a case to match a test, or
   deleting a case because its test was removed, defeats the gate: retire cases
   deliberately, in the inventory, in the same commit.
2. The gate only scans the modules in ``PORTED_MODULES``. A new test module is
   unchecked until it is listed here — add it at the same time as the file.
"""

import importlib
import inspect
import unittest

from tests import case_inventory

# Modules whose tests are checked against the inventory. test_live.py is
# deliberately absent: it needs a real daemon and cannot run in the default
# offline path.
PORTED_MODULES = [
    "tests.test_contract_positive",
    "tests.test_contract_negative",
    "tests.test_cli_positive",
    "tests.test_cli_negative",
    "tests.test_functions_positive",
    "tests.test_functions_negative",
]


def _iter_test_methods(module):
    """Yield (class name, method name, class object) for real tests in a module."""
    loader = unittest.TestLoader()
    for class_name, klass in inspect.getmembers(module, inspect.isclass):
        if klass.__module__ != module.__name__:
            continue
        if not issubclass(klass, unittest.TestCase):
            continue
        for method_name in loader.getTestCaseNames(klass):
            yield class_name, method_name, klass


def collect():
    """Return (test ids, case -> test id map, problems) for every ported module."""
    test_ids = set()
    mapped = {}
    problems = []
    for module_name in PORTED_MODULES:
        module = importlib.import_module(module_name)
        cases = getattr(module, "CASES", None)
        if cases is None:
            problems.append(f"{module_name} declares no CASES mapping")
            continue
        unmapped_classes = set(getattr(module, "UNMAPPED", set()))

        for class_name, method_name, klass in _iter_test_methods(module):
            test_id = f"{class_name}.{method_name}"
            test_ids.add(test_id)
            case_id = cases.get(test_id)
            if case_id is None:
                if class_name in unmapped_classes:
                    continue
                problems.append(f"{module_name}: {test_id} is not mapped to a case")
                continue
            if case_id in mapped:
                problems.append(
                    f"case {case_id!r} is claimed by both {mapped[case_id]} and {test_id}"
                )
            mapped[case_id] = test_id

        for test_id, case_id in cases.items():
            class_name, _, method_name = test_id.partition(".")
            klass = getattr(module, class_name, None)
            if klass is None or not hasattr(klass, method_name):
                problems.append(
                    f"{module_name}: case {case_id!r} names a missing test {test_id}"
                )
    return test_ids, mapped, problems


class InventoryGateTests(unittest.TestCase):
    def setUp(self):
        self.test_ids, self.mapped, self.problems = collect()

    def test_every_test_is_mapped_to_a_case(self):
        """No test may exist without a recorded case."""
        self.assertEqual(self.problems, [])

    def test_every_inventory_case_has_a_test(self):
        """No recorded case may exist without a test."""
        missing = [case_id for case_id in case_inventory.CASE_IDS if case_id not in self.mapped]
        self.assertEqual(missing, [], f"cases with no test: {missing}")

    def test_no_duplicate_case_ids(self):
        """A duplicated case id silently loses coverage."""
        seen, duplicates = set(), []
        for case_id in case_inventory.CASE_IDS:
            if case_id in seen:
                duplicates.append(case_id)
            seen.add(case_id)
        self.assertEqual(duplicates, [], f"duplicate case ids: {duplicates}")

    def test_case_sections_and_kinds_are_declared(self):
        """Every case names a known section and a known kind."""
        bad = []
        for case_id, section, kind, _description in case_inventory.CASES:
            if section not in case_inventory.SECTIONS:
                bad.append(f"{case_id}: unknown section {section}")
            if kind not in (case_inventory.POSITIVE, case_inventory.NEGATIVE):
                bad.append(f"{case_id}: unknown kind {kind!r}")
        self.assertEqual(bad, [], "; ".join(bad))

    def test_every_case_has_a_description(self):
        """A case without a description documents nothing."""
        undescribed = [
            case_id for case_id, _s, _k, description in case_inventory.CASES if not description.strip()
        ]
        self.assertEqual(undescribed, [], f"undescribed cases: {undescribed}")

    def test_every_section_has_a_positive_and_a_negative_case(self):
        """A section that only tests the happy path is half-tested."""
        incomplete = []
        for section in sorted(case_inventory.SECTIONS):
            kinds = {kind for _c, sec, kind, _d in case_inventory.CASES if sec == section}
            if (
                case_inventory.POSITIVE not in kinds
                and section not in case_inventory.NEGATIVE_ONLY_SECTIONS
            ):
                incomplete.append(f"section {section}: no positive case")
            if case_inventory.NEGATIVE not in kinds:
                incomplete.append(f"section {section}: no negative case")
        self.assertEqual(incomplete, [], "; ".join(incomplete))
