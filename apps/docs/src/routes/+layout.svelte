<script lang="ts">
	import '../app.css';
	let { data, children } = $props();
</script>

<div class="shell">
	<nav class="tree">
		<div class="brand">OGA9<span> docs</span></div>
		<a href="/">Overview</a>
		{#each data.nav as doc}
			<a href="/{doc.slug}">{doc.title}</a>
		{/each}
	</nav>
	<main>{@render children()}</main>
</div>
