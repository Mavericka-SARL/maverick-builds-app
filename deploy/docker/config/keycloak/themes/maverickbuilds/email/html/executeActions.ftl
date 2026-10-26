<#-- The invitation. The names come from account attributes, so every
     sentence goes through FreeMarker's escaping, not kcSanitize (which
     decodes entities first and lets a link in a name through). -->
<#assign kind = (user.attributes.mvx_invitation)!"">
<#assign by = (user.attributes.mvx_invited_by)!"">
<#assign workspace = (user.attributes.mvx_workspace)!"">
<#import "template.ftl" as layout>
<@layout.emailLayout>
<#if kind == "signup">
<p>${msg("mvxSignupLead", realmName)}</p>
<#elseif kind != "invite" || !by?has_content>
<p>${msg("mvxAccountLead", realmName)}</p>
<#elseif workspace?has_content>
<p>${msg("mvxInviteLead", by, workspace, realmName)}</p>
<#else>
<p>${msg("mvxInviteNoWorkspaceLead", by, realmName)}</p>
</#if>
<p><a href="${link}">${msg("mvxChoosePassword")}</a></p>
<p>${msg("mvxLinkWorks", linkExpirationFormatter(linkExpiration))} ${msg((kind == "signup")?then("mvxSignupIgnore", "mvxInviteIgnore"))}</p>
</@layout.emailLayout>
