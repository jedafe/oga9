DELETE FROM role_permissions WHERE permission = 'platform.blog.manage';
DELETE FROM permissions WHERE node = 'platform.blog.manage';
DROP TABLE blog_posts;
