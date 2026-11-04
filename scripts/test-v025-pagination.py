#!/usr/bin/env python3
"""Offline Link-header validation tests; live GitHub state is not simulated."""
import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch

path = Path(__file__).resolve().parents[1] / '.github/scripts/check-v025-acceptance.py'
spec = importlib.util.spec_from_file_location('acceptance', path)
acceptance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(acceptance)
BASE = 'https://api.github.com/repositories/1380855867/issues/646/comments?per_page=2&page='


def response(link='', body='[{"id":1}]'):
    return 'HTTP/2.0 200 OK\nContent-Type: application/json\n' + (f'Link: {link}\n' if link else '') + '\n' + body


def link(page, rel, base=BASE):
    return f'<{base}{page}>; rel="{rel}"'


class PaginationTests(unittest.TestCase):
    def pages(self, responses):
        with patch.object(acceptance.subprocess, 'check_output', side_effect=responses) as gh:
            pages = acceptance.comment_pages('ali-shortcuts/nexaroute', 646, 1380855867, 2)
            return pages, gh.call_args_list

    def test_three_page_canonical_chain(self):
        pages, calls = self.pages([
            response(', '.join([link(2, 'next'), link(3, 'last')]), '[{"id":1},{"id":10}]'),
            response(', '.join([link(1, 'prev'), link(3, 'next'), link(3, 'last'), link(1, 'first')]), '[{"id":2},{"id":20}]'),
            response(', '.join([link(2, 'prev'), link(1, 'first')]), '[{"id":3}]'),
        ])
        self.assertEqual([p[0]['id'] for p in pages], [1, 2, 3])
        self.assertTrue(all(c.args[0][2].startswith('-i') for c in calls))
        self.assertTrue(all(c.args[0][-1].startswith('repos/ali-shortcuts/nexaroute/issues/646/comments?') for c in calls))

    def test_bad_links_fail_before_following(self):
        cases = [
            'invalid',
            link(2, 'next', BASE.replace('api.github.com', 'example.com')) + ', ' + link(2, 'last'),
            link(2, 'next', BASE.replace('/1380855867/', '/999/')) + ', ' + link(2, 'last'),
            link(2, 'next', BASE.replace('/646/', '/647/')) + ', ' + link(2, 'last'),
            link(2, 'next', BASE.replace('per_page=2', 'per_page=3')) + ', ' + link(2, 'last'),
            link(1, 'next') + ', ' + link(2, 'last'),
            link(3, 'next') + ', ' + link(3, 'last'),
            link(2, 'next'),
            link(2, 'next') + ', ' + link(2, 'next') + ', ' + link(2, 'last'),
            link(2, 'unknown'),
        ]
        for value in cases:
            with self.subTest(link=value), patch.object(acceptance.subprocess, 'check_output', return_value=response(value)) as gh:
                with self.assertRaises(ValueError):
                    acceptance.comment_pages('ali-shortcuts/nexaroute', 646, 1380855867, 2)
                self.assertEqual(gh.call_count, 1)

    def test_truncated_chain_and_malformed_payload(self):
        cases = [
            [response(link(2, 'next') + ', ' + link(3, 'last'), '[{"id":1},{"id":2}]'), response(link(1, 'prev') + ', ' + link(1, 'first'))],
            [response(body='[{"id":1,"id":2}]')],
            ['HTTP/2.0 200 OK\nmalformed-header\n\n[]'],
            [response(body='{}')],
        ]
        for outputs in cases:
            with self.subTest(outputs=outputs), self.assertRaises(ValueError):
                self.pages(outputs)


if __name__ == '__main__':
    unittest.main()
