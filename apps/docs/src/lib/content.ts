// Ref: oga9-engineering/sections/01-foundation-tech-stack.md (docs surface)
// Markdown-driven docs: content/*.md → nav tree + rendered pages (japa
// apps/docs pattern). Frontmatter: title, order, section.
import fs from 'node:fs';
import path from 'node:path';
import { marked } from 'marked';

const CONTENT = path.resolve('content');

export interface DocMeta {
	slug: string;
	title: string;
	order: number;
}

function parseFrontmatter(raw: string): { meta: Record<string, string>; body: string } {
	const m = raw.match(/^---\n([\s\S]*?)\n---\n([\s\S]*)$/);
	if (!m) return { meta: {}, body: raw };
	const meta: Record<string, string> = {};
	for (const line of m[1].split('\n')) {
		const i = line.indexOf(':');
		if (i > 0) meta[line.slice(0, i).trim()] = line.slice(i + 1).trim();
	}
	return { meta, body: m[2] };
}

export function navTree(): DocMeta[] {
	if (!fs.existsSync(CONTENT)) return [];
	return fs
		.readdirSync(CONTENT)
		.filter((f) => f.endsWith('.md'))
		.map((f) => {
			const { meta } = parseFrontmatter(fs.readFileSync(path.join(CONTENT, f), 'utf8'));
			return {
				slug: f.replace(/\.md$/, ''),
				title: meta.title ?? f.replace(/\.md$/, ''),
				order: Number(meta.order ?? 99)
			};
		})
		.sort((a, b) => a.order - b.order);
}

export function getDoc(slug: string): { title: string; html: string } | null {
	const file = path.join(CONTENT, `${slug}.md`);
	if (!fs.existsSync(file)) return null;
	const { meta, body } = parseFrontmatter(fs.readFileSync(file, 'utf8'));
	return { title: meta.title ?? slug, html: marked(body) as string };
}
