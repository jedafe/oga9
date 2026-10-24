import { error } from '@sveltejs/kit';
import { getDoc } from '$lib/content';

export const load = ({ params }) => {
	const doc = getDoc(params.slug);
	if (!doc) throw error(404, 'doc not found');
	return doc;
};
