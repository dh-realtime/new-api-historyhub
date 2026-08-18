/* Minimal dependency-free markdown renderer for chat messages.
 * Escapes ALL html first, then transforms a practical subset:
 * fenced code (with copy button), inline code, headings, bold/italic/strike,
 * links, images (clickable lightbox), lists (one nesting level), blockquote,
 * hr, tables, paragraph line breaks. */
(function (global) {
	'use strict';

	function esc(s) {
		return String(s)
			.replace(/&/g, '&amp;')
			.replace(/</g, '&lt;')
			.replace(/>/g, '&gt;')
			.replace(/"/g, '&quot;');
	}

	/* Only http(s), same-origin paths and inline media data-urls may appear in
	 * href/src — blocks javascript: and other schemes. */
	function safeUrl(u) {
		u = String(u || '').trim();
		if (/^(https?:|data:image\/|data:video\/|data:audio\/)/i.test(u)) return u;
		if (u.charAt(0) === '/') return u;
		return '';
	}

	function renderInline(s) {
		// images before links: ![alt](url)
		s = s.replace(/!\[([^\]]*)\]\(([^)\s]+)\)/g, function (m, alt, url) {
			var u = safeUrl(url);
			if (!u) return m;
			return '<img class="md-img" src="' + u + '" alt="' + alt + '" loading="lazy" data-lightbox="1">';
		});
		s = s.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, function (m, t, url) {
			var u = safeUrl(url);
			if (!u || /^data:/i.test(u)) return t;
			return '<a href="' + u + '" target="_blank" rel="noopener">' + t + '</a>';
		});
		// bare urls (not already inside a tag produced above: those are preceded by a quote)
		s = s.replace(/(^|[\s(])(https?:\/\/[^\s<)]+)/g, function (m, p, u) {
			return p + '<a href="' + u + '" target="_blank" rel="noopener">' + u + '</a>';
		});
		s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
		s = s.replace(/(^|[^*\w])\*([^*\n]+)\*/g, '$1<em>$2</em>');
		s = s.replace(/~~([^~]+)~~/g, '<del>$1</del>');
		s = s.replace(/`([^`\n]+)`/g, '<code>$1</code>');
		return s;
	}

	function codeBlock(lang, code) {
		return (
			'<div class="md-code"><div class="md-code-bar"><span>' +
			(lang ? esc(lang) : 'text') +
			'</span><button class="md-copy" onclick="window.__copyCode(this)" type="button">复制</button></div>' +
			'<pre><code>' +
			code +
			'</code></pre></div>'
		);
	}

	function renderMarkdown(text) {
		var src = esc(String(text == null ? '' : text).replace(/\r\n?/g, '\n'));
		var lines = src.split('\n');
		var out = [];
		var para = [];

		function flushPara() {
			if (!para.length) return;
			out.push('<p>' + renderInline(para.join('<br>')) + '</p>');
			para = [];
		}

		var i = 0;
		while (i < lines.length) {
			var line = lines[i];

			// fenced code
			var fence = line.match(/^```(\w*)\s*$/);
			if (fence) {
				flushPara();
				var buf = [];
				i++;
				while (i < lines.length && !/^```\s*$/.test(lines[i])) {
					buf.push(lines[i]);
					i++;
				}
				i++; // closing fence (or EOF)
				out.push(codeBlock(fence[1], buf.join('\n')));
				continue;
			}

			// heading
			var h = line.match(/^(#{1,6})\s+(.*)$/);
			if (h) {
				flushPara();
				var lv = h[1].length;
				out.push('<h' + lv + '>' + renderInline(h[2]) + '</h' + lv + '>');
				i++;
				continue;
			}

			// hr
			if (/^\s*(-{3,}|\*{3,}|_{3,})\s*$/.test(line)) {
				flushPara();
				out.push('<hr>');
				i++;
				continue;
			}

			// table: |a|b| then |---|---|
			if (/^\s*\|.*\|\s*$/.test(line) && i + 1 < lines.length && /^\s*\|[\s:|-]+\|\s*$/.test(lines[i + 1])) {
				flushPara();
				var head = line.trim().replace(/^\||\|$/g, '').split('|');
				var rows = [];
				i += 2;
				while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) {
					rows.push(lines[i].trim().replace(/^\||\|$/g, '').split('|'));
					i++;
				}
				var t = '<div class="md-table-wrap"><table><thead><tr>';
				for (var c = 0; c < head.length; c++) t += '<th>' + renderInline(head[c].trim()) + '</th>';
				t += '</tr></thead><tbody>';
				for (var r = 0; r < rows.length; r++) {
					t += '<tr>';
					for (var c2 = 0; c2 < head.length; c2++) t += '<td>' + renderInline((rows[r][c2] || '').trim()) + '</td>';
					t += '</tr>';
				}
				out.push(t + '</tbody></table></div>');
				continue;
			}

			// blockquote
			if (/^\s*>/.test(line)) {
				flushPara();
				var q = [];
				while (i < lines.length && /^\s*>/.test(lines[i])) {
					q.push(lines[i].replace(/^\s*>\s?/, ''));
					i++;
				}
				out.push('<blockquote>' + renderMarkdown(q.join('\n')) + '</blockquote>');
				continue;
			}

			// lists (one nesting level via 2+ leading spaces)
			if (/^\s*([-*+]|\d+[.)])\s+/.test(line)) {
				flushPara();
				var ordered = /^\s*\d+[.)]\s+/.test(line);
				var tag = ordered ? 'ol' : 'ul';
				var html = '<' + tag + '>';
				while (i < lines.length && /^\s*([-*+]|\d+[.)])\s+/.test(lines[i])) {
					var m = lines[i].match(/^(\s*)([-*+]|\d+[.)])\s+(.*)$/);
					if (m[1].length >= 2) html += '<li class="sub"><ul><li>' + renderInline(m[3]) + '</li></ul></li>';
					else html += '<li>' + renderInline(m[3]) + '</li>';
					i++;
				}
				out.push(html + '</' + tag + '>');
				continue;
			}

			if (line.trim() === '') {
				flushPara();
				i++;
				continue;
			}

			para.push(line);
			i++;
		}
		flushPara();
		return out.join('\n');
	}

	window.__copyCode = function (btn) {
		var code = btn.closest('.md-code').querySelector('code');
		var text = code ? code.innerText : '';
		if (navigator.clipboard && navigator.clipboard.writeText) {
			navigator.clipboard.writeText(text).then(function () {
				btn.textContent = '已复制';
				setTimeout(function () { btn.textContent = '复制'; }, 1500);
			});
		} else {
			var ta = document.createElement('textarea');
			ta.value = text;
			document.body.appendChild(ta);
			ta.select();
			document.execCommand('copy');
			document.body.removeChild(ta);
			btn.textContent = '已复制';
			setTimeout(function () { btn.textContent = '复制'; }, 1500);
		}
	};

	global.renderMarkdown = renderMarkdown;
})(window);
